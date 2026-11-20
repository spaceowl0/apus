package peval

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"apus/internal/ir"
	"apus/internal/syntax"
	"apus/internal/types"
)

func (e *eng) global(name string) *V {
	if v, ok := e.gconst[name]; ok {
		return v
	}
	cd := e.info.Consts[name]
	if cd == nil {
		return nil
	}
	if e.gbusy[name] {
		e.fail("E0307", "const `%s` depends on itself", name)
	}
	if dv, ok := e.opt.Defines[name]; ok {
		var v *V
		if cd.Ty.Kind == types.KBool {
			v = cBool(dv.Sign() != 0)
		} else {
			v = cInt(cd.Ty, dv)
		}
		e.gconst[name] = v
		return v
	}
	e.gbusy[name] = true
	saved := e.pos
	v := e.expr(newEnv(), cd.X)
	if !allConst(v) {
		e.fail("E0101", "const `%s` is not a compile-time constant", name)
	}
	e.pos = saved
	e.gbusy[name] = false
	e.gconst[name] = v
	return v
}

func (e *eng) expr(en *env, x syntax.Expr) *V {
	e.tick(x.Position())
	switch n := x.(type) {
	case *syntax.IntLit:
		if t := fixTy(n.Ty); t.Kind == types.KFloat {
			f, _ := new(big.Float).SetInt(n.V).Float64()
			return cFloat(t, f)
		}
		return cInt(fixTy(n.Ty), n.V)
	case *syntax.FloatLit:
		t := fixTy(n.Ty)
		f, _ := strconv.ParseFloat(n.Text, t.Bits)
		return cFloat(t, f)
	case *syntax.StrLit:
		return cStr(n.V)
	case *syntax.BoolLit:
		return cBool(n.V)
	case *syntax.Ident:
		if n.EnumVal != nil {
			return cInt(n.Ty, n.EnumVal)
		}
		if b := en.lookup(n.Name); b != nil {
			return b.v
		}
		if v := e.global(n.Name); v != nil {
			return v
		}
		e.fail("E0004", "unknown name `%s`", n.Name)
	case *syntax.Static:
		v := e.expr(en, n.X)
		e.pos = n.Pos
		if !allConst(v) {
			e.failh("E0101", "a runtime value reaches this expression; bound it or remove `static`",
				"`static` expression did not fold to a compile-time constant")
		}
		return v
	case *syntax.Unary:
		v := e.expr(en, n.X)
		e.pos = n.Pos
		return e.unary(n.Op, v, fixTy(n.Ty))
	case *syntax.Binary:
		return e.binary(en, n)
	case *syntax.Cast:
		v := e.expr(en, n.X)
		e.pos = n.Pos
		return e.cast(v, n.Ty)
	case *syntax.Index:
		return e.index(en, n)
	case *syntax.Field:
		if ix, ok := n.X.(*syntax.Index); ok {
			if at := ix.X.Type(); at != nil && at.Kind == types.KArray && at.Elem.Kind == types.KStruct {
				return e.structArrayField(en, ix, n)
			}
		}
		base := e.expr(en, n.X)
		return base.elems[n.Index]
	case *syntax.StructLit:
		st := n.Ty
		out := &V{k: kStruct, ty: st, elems: make([]*V, len(st.Fields))}
		for _, fi := range n.Fields {
			fidx := st.FieldIndex(fi.Name)
			out.elems[fidx] = withCap(e.own(e.expr(en, fi.X)), st.Fields[fidx].Ty)
		}
		return out
	case *syntax.ArrayLit:
		out := &V{k: kArr, ty: n.Ty, elems: make([]*V, len(n.Elems))}
		for i, el := range n.Elems {
			out.elems[i] = withCap(e.own(e.expr(en, el)), n.Ty.Elem)
		}
		return out
	case *syntax.ArrayRep:
		val := e.expr(en, n.Val)
		out := &V{k: kArr, ty: n.Ty, elems: make([]*V, n.Len)}
		for i := range out.elems {
			out.elems[i] = withCap(e.own(val), n.Ty.Elem)
		}
		return out
	case *syntax.MatchExpr:
		return e.matchExpr(en, n)
	case *syntax.Call:
		return e.call(en, n)
	}
	e.fail("E0000", "internal: unhandled expression %T", x)
	return nil
}

func (e *eng) unary(op string, v *V, ty *types.Type) *V {
	if ty.Kind == types.KFloat && op == "-" {
		if v.k == kConst {
			e.stats.FoldedOps++
			return cFloat(ty, -v.f)
		}
		id := e.newVar()
		e.emit(ir.FUn{Dst: id, Op: ir.FNeg, A: opnd(v), F32: ty.Bits == 32})
		return dynScalar(ty, id, nil, nil)
	}
	if op == "!" {
		if v.k == kConst {
			e.stats.FoldedOps++
			return cBool(v.c.Sign() == 0)
		}
		id := e.newVar()
		e.emit(ir.Not{Dst: id, A: opnd(v)})
		return dynScalar(types.Bool, id, nil, nil)
	}
	if v.k == kConst {
		r := new(big.Int).Neg(v.c)
		if !inRange(r, ty) {
			e.fail("E0301", "arithmetic overflow: -(%s) does not fit in `%s`", v.c, ty)
		}
		e.stats.FoldedOps++
		return cInt(ty, r)
	}
	lo, hi := new(big.Int).Neg(v.hi), new(big.Int).Neg(v.lo)
	tlo, thi := ty.Range()
	check := lo.Cmp(tlo) < 0 || hi.Cmp(thi) > 0
	if lo.Cmp(thi) > 0 || hi.Cmp(tlo) < 0 {
		return e.alwaysTraps(ty, "negation overflow:")
	}
	id := e.newVar()
	e.emit(ir.Neg{Dst: id, A: opnd(v), Ty: irTy(ty), Check: check})
	return dynScalar(ty, id, lo, hi)
}

func (e *eng) binary(en *env, n *syntax.Binary) *V {
	switch n.Op {
	case "&&", "||":
		a := e.expr(en, n.L)
		e.pos = n.Pos
		isAnd := n.Op == "&&"
		if a.k == kConst {
			e.stats.BranchesRemoved++
			if (a.c.Sign() != 0) == isAnd {
				return e.expr(en, n.R)
			}
			return a
		}
		slot := e.newVar()
		e.emit(ir.Mov{Dst: slot, A: opnd(a)})
		var rv *V
		body := e.capture(func() {
			undo := e.refine(en, n.L, isAnd) // right side only runs when the left is true (&&) / false (||)
			rv = e.expr(en, n.R)
			undo()
			e.emit(ir.Mov{Dst: slot, A: opnd(rv)})
		})
		if rv.k == kConst && len(body) == 1 {
			// a && true == a ; a || false == a ; a && false == false ; a || true == true
			if (rv.c.Sign() != 0) == isAnd {
				return a
			}
			return rv
		}
		cond := ir.V(slot)
		if isAnd {
			e.emit(ir.If{Cond: cond, Then: body})
		} else {
			e.emit(ir.If{Cond: cond, Else: body})
		}
		return dynScalar(types.Bool, slot, nil, nil)
	}
	a := e.expr(en, n.L)
	b := e.expr(en, n.R)
	e.pos = n.Pos
	if n.Op == "++" {
		return e.concat(a, b)
	}
	if n.L.Type().Kind == types.KStr {
		return e.strEq(n.Op == "!=", a, b)
	}
	opTy := fixTy(n.L.Type())
	if opTy.Kind == types.KUntyped {
		opTy = types.I32
	}
	if opTy.Kind == types.KFloat {
		return e.floatBin(n.Op, a, b, opTy)
	}
	switch n.Op {
	case "==", "!=", "<", "<=", ">", ">=":
		return e.compare(n.Op, a, b, opTy)
	}
	return e.arith(n.Op, a, b, fixTy(n.Ty))
}

func (e *eng) concat(a, b *V) *V {
	var parts []part
	add := func(v *V) {
		switch v.k {
		case kConst:
			parts = append(parts, part{s: v.s})
		case kDyn:
			parts = append(parts, part{d: v})
		case kRope:
			parts = append(parts, v.parts...)
		}
	}
	add(a)
	add(b)
	var merged []part
	for _, p := range parts {
		if p.d == nil && len(merged) > 0 && merged[len(merged)-1].d == nil {
			merged[len(merged)-1].s += p.s
		} else if p.d == nil && p.s == "" {
			continue
		} else {
			merged = append(merged, p)
		}
	}
	if len(merged) == 0 {
		return cStr("")
	}
	if len(merged) == 1 && merged[0].d == nil {
		e.stats.FoldedOps++
		return cStr(merged[0].s)
	}
	r := &V{k: kRope, parts: merged}
	r.ty = types.Str(strCap(r))
	return r
}

var irOps = map[string]ir.Op{
	"+": ir.Add, "-": ir.Sub, "*": ir.Mul, "/": ir.Div, "%": ir.Rem, "&": ir.And, "|": ir.Or, "^": ir.Xor,
	"<<": ir.Shl, ">>": ir.Shr, "==": ir.Eq, "!=": ir.Ne, "<": ir.Lt, "<=": ir.Le, ">": ir.Gt, ">=": ir.Ge,
}

func wrapBig(r *big.Int, t *types.Type) *big.Int {
	mask := new(big.Int).Lsh(big.NewInt(1), uint(t.Bits))
	mask.Sub(mask, big.NewInt(1))
	out := new(big.Int).And(r, mask)
	if t.Signed && out.Bit(t.Bits-1) == 1 {
		out.Sub(out, new(big.Int).Lsh(big.NewInt(1), uint(t.Bits)))
	}
	return out
}

func cmpFold(op string, alo, ahi, blo, bhi *big.Int) (res, ok bool) {
	switch op {
	case "<":
		if ahi.Cmp(blo) < 0 {
			return true, true
		}
		if alo.Cmp(bhi) >= 0 {
			return false, true
		}
	case "<=":
		if ahi.Cmp(blo) <= 0 {
			return true, true
		}
		if alo.Cmp(bhi) > 0 {
			return false, true
		}
	case ">":
		if alo.Cmp(bhi) > 0 {
			return true, true
		}
		if ahi.Cmp(blo) <= 0 {
			return false, true
		}
	case ">=":
		if alo.Cmp(bhi) >= 0 {
			return true, true
		}
		if ahi.Cmp(blo) < 0 {
			return false, true
		}
	case "==", "!=":
		eq, known := false, false
		if alo.Cmp(ahi) == 0 && blo.Cmp(bhi) == 0 && alo.Cmp(blo) == 0 {
			eq, known = true, true
		} else if ahi.Cmp(blo) < 0 || alo.Cmp(bhi) > 0 {
			eq, known = false, true
		}
		if known {
			return eq == (op == "=="), true
		}
	}
	return false, false
}

func (e *eng) compare(op string, a, b *V, ty *types.Type) *V {
	alo, ahi := rng(a)
	blo, bhi := rng(b)
	if a.k == kConst && b.k == kConst {
		e.stats.FoldedOps++
		c := a.c.Cmp(b.c)
		var r bool
		switch op {
		case "==":
			r = c == 0
		case "!=":
			r = c != 0
		case "<":
			r = c < 0
		case "<=":
			r = c <= 0
		case ">":
			r = c > 0
		case ">=":
			r = c >= 0
		}
		return cBool(r)
	}
	if r, ok := cmpFold(op, alo, ahi, blo, bhi); ok {
		e.stats.FoldedOps++
		return cBool(r)
	}
	id := e.newVar()
	e.emit(ir.Bin{Dst: id, Op: irOps[op], A: opnd(a), B: opnd(b), Ty: irTy(ty)})
	return dynScalar(types.Bool, id, nil, nil)
}

// nonZero reports whether v is provably different from zero.
func nonZero(v *V) bool {
	if v.k == kConst {
		return v.c.Sign() != 0
	}
	return v.lo.Sign() > 0 || v.hi.Sign() < 0 || (v.excl != nil && v.excl.Sign() == 0)
}

func isConstVal(v *V, n int64) bool { return v.k == kConst && v.c.IsInt64() && v.c.Int64() == n }

func (e *eng) arith(op string, a, b *V, ty *types.Type) *V {
	bits := ty.Bits
	if a.k == kConst && b.k == kConst {
		x, y := a.c, b.c
		r := new(big.Int)
		switch op {
		case "+":
			r.Add(x, y)
		case "-":
			r.Sub(x, y)
		case "*":
			r.Mul(x, y)
		case "/", "%":
			if y.Sign() == 0 {
				e.fail("E0303", "division by zero")
			}
			if op == "/" {
				r.Quo(x, y)
			} else {
				r.Rem(x, y)
			}
		case "&":
			r.And(x, y)
		case "|":
			r.Or(x, y)
		case "^":
			r.Xor(x, y)
		case "<<", ">>":
			if y.Sign() < 0 || y.Cmp(big.NewInt(int64(bits))) >= 0 {
				e.fail("E0301", "shift amount %s out of range for `%s`", y, ty)
			}
			if op == "<<" {
				r = wrapBig(r.Lsh(x, uint(y.Uint64())), ty)
			} else {
				r.Rsh(x, uint(y.Uint64()))
			}
		}
		if !inRange(r, ty) {
			e.failh("E0301", "use a wider type or a wrapping operation",
				"arithmetic overflow: %s %s %s does not fit in `%s`", x, op, y, ty)
		}
		e.stats.FoldedOps++
		return cInt(ty, r)
	}
	// algebraic identities with one constant operand
	switch op {
	case "+", "|", "^":
		if isConstVal(b, 0) {
			return a
		}
		if isConstVal(a, 0) {
			return b
		}
	case "-", "<<", ">>":
		if isConstVal(b, 0) {
			return a
		}
	case "*":
		if isConstVal(b, 1) {
			return a
		}
		if isConstVal(a, 1) {
			return b
		}
		if isConstVal(a, 0) || isConstVal(b, 0) {
			return cInt(ty, big.NewInt(0))
		}
	case "/":
		if isConstVal(b, 1) {
			return a
		}
	case "&":
		if isConstVal(a, 0) || isConstVal(b, 0) {
			return cInt(ty, big.NewInt(0))
		}
	}
	alo, ahi := rng(a)
	blo, bhi := rng(b)
	tlo, thi := ty.Range()
	var lo, hi *big.Int
	check := false
	zero := big.NewInt(0)
	switch op {
	case "+":
		lo, hi = new(big.Int).Add(alo, blo), new(big.Int).Add(ahi, bhi)
	case "-":
		lo, hi = new(big.Int).Sub(alo, bhi), new(big.Int).Sub(ahi, blo)
	case "*":
		ps := []*big.Int{new(big.Int).Mul(alo, blo), new(big.Int).Mul(alo, bhi), new(big.Int).Mul(ahi, blo), new(big.Int).Mul(ahi, bhi)}
		lo, hi = ps[0], ps[0]
		for _, p := range ps[1:] {
			lo, hi = bmin(lo, p), bmax(hi, p)
		}
	case "/":
		if blo.Sign() > 0 || bhi.Sign() < 0 {
			qs := []*big.Int{new(big.Int).Quo(alo, blo), new(big.Int).Quo(alo, bhi), new(big.Int).Quo(ahi, blo), new(big.Int).Quo(ahi, bhi)}
			lo, hi = qs[0], qs[0]
			for _, q := range qs[1:] {
				lo, hi = bmin(lo, q), bmax(hi, q)
			}
		} else {
			m := bmax(babs(alo), babs(ahi))
			lo, hi = new(big.Int).Neg(m), m
		}
		if !nonZero(b) {
			check = true
		}
	case "%":
		if !nonZero(b) {
			check = true
		}
		m := bmax(babs(blo), babs(bhi))
		lim := new(big.Int).Sub(m, big.NewInt(1))
		if lim.Sign() < 0 {
			lim = zero
		}
		switch {
		case alo.Sign() >= 0:
			lo, hi = zero, bmin(lim, ahi)
		case ahi.Sign() <= 0:
			lo, hi = bmax(new(big.Int).Neg(lim), alo), zero
		default:
			lo, hi = bmax(new(big.Int).Neg(lim), alo), bmin(lim, ahi)
		}
	case "&":
		switch {
		case alo.Sign() >= 0 && blo.Sign() >= 0:
			lo, hi = zero, bmin(ahi, bhi)
		case alo.Sign() >= 0:
			lo, hi = zero, ahi
		case blo.Sign() >= 0:
			lo, hi = zero, bhi
		default:
			lo, hi = tlo, thi
		}
	case "|", "^":
		if alo.Sign() >= 0 && blo.Sign() >= 0 {
			m := bmax(ahi, bhi)
			mask := new(big.Int).Lsh(big.NewInt(1), uint(m.BitLen()))
			mask.Sub(mask, big.NewInt(1))
			lo, hi = zero, mask
			if op == "|" {
				lo = bmax(alo, blo)
			}
		} else {
			lo, hi = tlo, thi
		}
	case "<<":
		lo, hi = tlo, thi
		if b.k == kConst {
			k := uint(b.c.Uint64())
			l, h := new(big.Int).Lsh(alo, k), new(big.Int).Lsh(ahi, k)
			if l.Cmp(tlo) >= 0 && h.Cmp(thi) <= 0 {
				lo, hi = l, h
			}
		}
		if blo.Sign() < 0 || bhi.Cmp(big.NewInt(int64(bits))) >= 0 {
			check = true
		}
	case ">>":
		if b.k == kConst {
			k := uint(b.c.Uint64())
			lo, hi = new(big.Int).Rsh(alo, k), new(big.Int).Rsh(ahi, k)
		} else {
			lo, hi = zero, bmax(ahi, zero)
			if alo.Sign() < 0 {
				lo = alo
				hi = bmax(ahi, big.NewInt(-1))
			}
		}
		if blo.Sign() < 0 || bhi.Cmp(big.NewInt(int64(bits))) >= 0 {
			check = true
		}
	}
	if op != "<<" && op != ">>" && (lo.Cmp(tlo) < 0 || hi.Cmp(thi) > 0) {
		if lo.Cmp(thi) > 0 || hi.Cmp(tlo) < 0 {
			return e.alwaysTraps(ty, "arithmetic overflow:")
		}
		check = true
	}
	id := e.newVar()
	e.emit(ir.Bin{Dst: id, Op: irOps[op], A: opnd(a), B: opnd(b), Ty: irTy(ty), Check: check})
	return dynScalar(ty, id, lo, hi)
}

var fops = map[string]ir.FOp{"+": ir.FAdd, "-": ir.FSub, "*": ir.FMul, "/": ir.FDiv,
	"==": ir.FEq, "!=": ir.FNe, "<": ir.FLt, "<=": ir.FLe, ">": ir.FGt, ">=": ir.FGe}

// floatBin folds a float operation using the shared IEEE semantics, or emits it.
func (e *eng) floatBin(op string, a, b *V, ty *types.Type) *V {
	f32 := ty.Bits == 32
	fo := fops[op]
	if a.k == kConst && b.k == kConst {
		e.stats.FoldedOps++
		r := ir.EvalFBin(fo, vbits(a), vbits(b), f32)
		if fo.IsCmp() {
			return cBool(r != 0)
		}
		return cFloat(ty, ir.ToFloat64(r, f32))
	}
	id := e.newVar()
	e.emit(ir.FBin{Dst: id, Op: fo, A: opnd(a), B: opnd(b), F32: f32})
	if fo.IsCmp() {
		return dynScalar(types.Bool, id, nil, nil)
	}
	return dynScalar(ty, id, nil, nil)
}

func bigFromBits(bits uint64, t *types.Type) *big.Int {
	if t.Signed {
		return big.NewInt(int64(bits))
	}
	return new(big.Int).SetUint64(bits)
}

// castFloat handles every conversion that involves a float on either side.
func (e *eng) castFloat(v *V, to *types.Type) *V {
	from := v.ty
	switch {
	case from.IsFloat() && to.IsFloat():
		if from.Bits == to.Bits {
			return v
		}
		if v.k == kConst {
			e.stats.FoldedOps++
			return cFloat(to, v.f)
		}
		id := e.newVar()
		e.emit(ir.FCvt{Dst: id, A: opnd(v), Kind: ir.CvtFloatToFloat, F32: from.Bits == 32})
		return dynScalar(to, id, nil, nil)
	case from.IsFloat():
		if v.k == kConst {
			bits, ok := ir.FloatToInt(vbits(v), from.Bits == 32, irTy(to))
			if !ok {
				e.failh("E0301", "use a wider integer type or clamp the float first",
					"float value %s cannot be converted to `%s` (NaN or out of range)", ir.FormatFloat(v.f), to)
			}
			e.stats.FoldedOps++
			return cInt(to, bigFromBits(bits, to))
		}
		id := e.newVar()
		e.emit(ir.FCvt{Dst: id, A: opnd(v), Kind: ir.CvtFloatToInt, Int: irTy(to), F32: from.Bits == 32})
		return dynScalar(to, id, nil, nil)
	}
	// integer (or enum) to float
	if v.k == kConst {
		e.stats.FoldedOps++
		bits := ir.IntToFloat(toBits(v.c, from), irTy(from), to.Bits == 32)
		return cFloat(to, ir.ToFloat64(bits, to.Bits == 32))
	}
	id := e.newVar()
	e.emit(ir.FCvt{Dst: id, A: opnd(v), Kind: ir.CvtIntToFloat, Int: irTy(from), F32: to.Bits == 32})
	return dynScalar(to, id, nil, nil)
}

func (e *eng) cast(v *V, to *types.Type) *V {
	to = fixTy(to)
	if v.ty.IsFloat() || to.IsFloat() {
		return e.castFloat(v, to)
	}
	if v.k == kConst {
		if !inRange(v.c, to) {
			e.failh("E0301", "use `clamp`/`require` or a wider type",
				"cast of %s to `%s` does not fit", v.c, to)
		}
		e.stats.FoldedOps++
		return cInt(to, v.c)
	}
	tlo, thi := to.Range()
	if v.lo.Cmp(tlo) >= 0 && v.hi.Cmp(thi) <= 0 {
		return &V{k: kDyn, ty: to, v: v.v, lo: v.lo, hi: v.hi, slot: v.slot}
	}
	if v.lo.Cmp(thi) > 0 || v.hi.Cmp(tlo) < 0 {
		return e.alwaysTraps(to, "cast out of range:")
	}
	id := e.newVar()
	e.emit(ir.Cast{Dst: id, A: opnd(v), From: irTy(v.ty), To: irTy(to)})
	return dynScalar(to, id, bmax(v.lo, tlo), bmin(v.hi, thi))
}

func (e *eng) bytesV(s string) *V {
	out := &V{k: kArr, ty: types.Array(types.U8, len(s)), elems: make([]*V, len(s))}
	for i := 0; i < len(s); i++ {
		out.elems[i] = cInt(types.U8, big.NewInt(int64(s[i])))
	}
	return out
}

// materializeRope builds a rope into a fresh runtime buffer so it can be indexed,
// compared and measured like any other runtime string.
func (e *eng) materializeRope(v *V) *V {
	if v.mat != nil && (v.matReg == 0 || e.inRegion(v.matReg)) {
		return v.mat
	}
	capacity := strCap(v)
	if capacity < 1 {
		capacity = 1
	}
	buf, ln := e.newBuf(capacity), e.newVar()
	parts := make([]ir.StrPart, 0, len(v.parts))
	for _, p := range v.parts {
		switch {
		case p.d == nil:
			parts = append(parts, ir.StrPart{Kind: 0, S: p.s})
		case p.d.ty.Kind == types.KStr:
			parts = append(parts, ir.StrPart{Kind: 1, Buf: p.d.buf, Len: ir.V(p.d.v)})
		case p.d.ty.Kind == types.KFloat:
			parts = append(parts, ir.StrPart{Kind: 3, A: opnd(p.d), F32: p.d.ty.Bits == 32})
		default:
			parts = append(parts, ir.StrPart{Kind: 2, A: opnd(p.d), Ty: irTy(p.d.ty)})
		}
	}
	e.emit(ir.StrBuild{Buf: buf, Len: ln, Parts: parts})
	m := &V{k: kDyn, ty: types.Str(strCap(v)), v: ln, buf: buf, lo: big.NewInt(0), hi: big.NewInt(int64(strCap(v)))}
	v.mat, v.matReg = m, e.region()
	return m
}

// withCap records the declared capacity of a string variable/field/element on v, so a
// later slot (see strSlot) is large enough for anything the declared type allows.
func withCap(v *V, t *types.Type) *V {
	if t == nil || t.Kind != types.KStr || v.ty == nil || v.ty.Kind != types.KStr || v.capDecl >= t.Cap {
		return v
	}
	c := *v
	c.capDecl = t.Cap
	return &c
}

// strParts lowers any string value to StrBuild parts.
func (e *eng) strParts(v *V) []ir.StrPart {
	switch v.k {
	case kConst:
		return []ir.StrPart{{Kind: 0, S: v.s}}
	case kDyn:
		return []ir.StrPart{{Kind: 1, Buf: v.buf, Len: ir.V(v.v)}}
	}
	parts := make([]ir.StrPart, 0, len(v.parts))
	for _, p := range v.parts {
		switch {
		case p.d == nil:
			parts = append(parts, ir.StrPart{Kind: 0, S: p.s})
		case p.d.ty.Kind == types.KStr:
			parts = append(parts, ir.StrPart{Kind: 1, Buf: p.d.buf, Len: ir.V(p.d.v)})
		case p.d.ty.Kind == types.KFloat:
			parts = append(parts, ir.StrPart{Kind: 3, A: opnd(p.d), F32: p.d.ty.Bits == 32})
		default:
			parts = append(parts, ir.StrPart{Kind: 2, A: opnd(p.d), Ty: irTy(p.d.ty)})
		}
	}
	return parts
}

// strSlot copies a string into a fresh fixed-capacity buffer whose contents can
// be rewritten by assignments inside dynamic control flow.
func (e *eng) strSlot(v *V) *V {
	capacity := strCap(v)
	if v.capDecl > capacity {
		capacity = v.capDecl
	}
	if capacity < 1 {
		capacity = 1
	}
	buf, ln := e.newBuf(capacity), e.newVar()
	e.emit(ir.StrBuild{Buf: buf, Len: ln, Parts: e.strParts(v)})
	return &V{k: kDyn, ty: types.Str(capacity), v: ln, buf: buf, lo: big.NewInt(0), hi: big.NewInt(int64(capacity)), slot: true, capDecl: capacity}
}

// copyStr snapshots a slot-backed string so later assignments do not alias it.
func (e *eng) copyStr(v *V) *V {
	buf, ln := e.newBuf(v.ty.Cap), e.newVar()
	e.emit(ir.StrBuild{Buf: buf, Len: ln, Parts: e.strParts(v)})
	return &V{k: kDyn, ty: v.ty, v: ln, buf: buf, lo: big.NewInt(0), hi: big.NewInt(int64(v.ty.Cap))}
}

// assignStr rewrites a slot-backed string in place. A value that reads the slot's
// own buffer is staged through a temporary so overlapping copies stay correct.
func (e *eng) assignStr(cur, nv *V) {
	if strCap(nv) > cur.ty.Cap {
		e.fail("E0502", "assigned string may need %d bytes but the variable holds at most %d", strCap(nv), cur.ty.Cap)
	}
	parts := e.strParts(nv)
	for _, p := range parts {
		if p.Kind == 1 && p.Buf == cur.buf {
			tb, tl := e.newBuf(strCap(nv)), e.newVar()
			e.emit(ir.StrBuild{Buf: tb, Len: tl, Parts: parts})
			parts = []ir.StrPart{{Kind: 1, Buf: tb, Len: ir.V(tl)}}
			break
		}
	}
	e.emit(ir.StrBuild{Buf: cur.buf, Len: cur.v, Parts: parts})
}

func (e *eng) strEq(neg bool, a, b *V) *V {
	if a.k == kConst && b.k == kConst {
		e.stats.FoldedOps++
		return cBool((a.s == b.s) != neg)
	}
	if a.k == kRope {
		a = e.materializeRope(a)
	}
	if b.k == kRope {
		b = e.materializeRope(b)
	}
	if a.k == kDyn && b.k == kDyn {
		id := e.newVar()
		e.emit(ir.BufEq{Dst: id, BufA: a.buf, LenA: ir.V(a.v), BufB: b.buf, LenB: ir.V(b.v)})
		r := dynScalar(types.Bool, id, nil, nil)
		if neg {
			nid := e.newVar()
			e.emit(ir.Not{Dst: nid, A: ir.V(id)})
			r = dynScalar(types.Bool, nid, nil, nil)
		}
		return r
	}
	d, c := a, b
	if a.k == kConst {
		d, c = b, a
	}
	if len(c.s) > d.ty.Cap {
		e.stats.FoldedOps++
		return cBool(neg)
	}
	id := e.newVar()
	e.emit(ir.StrEq{Dst: id, Buf: d.buf, Len: ir.V(d.v), S: c.s})
	r := dynScalar(types.Bool, id, nil, nil)
	if neg {
		nid := e.newVar()
		e.emit(ir.Not{Dst: nid, A: ir.V(id)})
		r = dynScalar(types.Bool, nid, nil, nil)
	}
	return r
}

func (e *eng) strIndex(base, idx *V) *V {
	switch base.k {
	case kConst:
		n := len(base.s)
		if idx.k == kConst {
			if idx.c.Sign() < 0 || idx.c.Cmp(big.NewInt(int64(n))) >= 0 {
				e.failh("E0304", "valid indices are 0..", "index %s is out of range for a string of length %d", idx.c, n)
			}
			e.stats.FoldedOps++
			return cInt(types.U8, big.NewInt(int64(base.s[idx.c.Int64()])))
		}
		ilo, ihi := rng(idx)
		if n == 0 || ihi.Sign() < 0 || ilo.Cmp(big.NewInt(int64(n))) >= 0 {
			e.fail("E0304", "index is always out of range for a string of length %d", n)
		}
		check := ilo.Sign() < 0 || ihi.Cmp(big.NewInt(int64(n-1))) > 0
		tab, lo, hi := e.constTable(e.bytesV(base.s))
		id := e.newVar()
		e.emit(ir.TLoad{Dst: id, Tab: tab, Idx: opnd(idx), Check: check})
		return dynScalar(types.U8, id, lo, hi)
	case kDyn:
		if idx.k == kConst && (idx.c.Sign() < 0 || idx.c.Cmp(big.NewInt(int64(base.ty.Cap))) >= 0) {
			e.failh("E0304", "valid indices are 0..", "index %s is out of range for a string of capacity %d", idx.c, base.ty.Cap)
		}
		id := e.newVar()
		e.emit(ir.BufLoad{Dst: id, Buf: base.buf, Idx: opnd(idx), Len: ir.V(base.v)})
		return dynScalar(types.U8, id, nil, nil)
	}
	if base.k == kRope {
		return e.strIndex(e.materializeRope(base), idx)
	}
	e.fail("E0000", "internal: indexing an unexpected string value")
	return nil
}

func (e *eng) index(en *env, n *syntax.Index) *V {
	var base *V
	if id, ok := n.X.(*syntax.Ident); ok && en.lookup(id.Name) == nil {
		base = e.global(id.Name)
	}
	if base == nil {
		base = e.expr(en, n.X)
	}
	idx := e.expr(en, n.I)
	e.pos = n.Pos
	if base.ty.Kind == types.KStr {
		return e.strIndex(base, idx)
	}
	if base.k == kSoA {
		return e.soaElem(base, idx)
	}
	return e.indexValue(base, idx)
}

// indexValue indexes an array value (tracked elements or IR memory) with idx.
func (e *eng) indexValue(base, idx *V) *V {
	N := int64(base.ty.N)
	ilo, ihi := rng(idx)
	if idx.k == kConst {
		if idx.c.Sign() < 0 || idx.c.Cmp(big.NewInt(N)) >= 0 {
			e.failh("E0304", "valid indices are 0..", "index %s is out of range for array of length %d", idx.c, N)
		}
		i := int(idx.c.Int64())
		if base.k == kArr {
			return base.elems[i]
		}
		id := e.newVar()
		e.emit(ir.Load{Dst: id, Arr: base.mem, Idx: opnd(idx)})
		return dynScalar(base.ty.Elem, id, base.elo, base.ehi)
	}
	if ihi.Sign() < 0 || ilo.Cmp(big.NewInt(N)) >= 0 {
		e.fail("E0304", "index is always out of range for array of length %d", N)
	}
	check := ilo.Sign() < 0 || ihi.Cmp(big.NewInt(N-1)) > 0
	if base.k == kArr && !base.ty.Elem.IsScalar() {
		return e.dynSelect(base.elems, base.ty.Elem, idx)
	}
	if base.k == kArr && base.ty.Elem.IsScalar() && allConst(base) {
		tab, elo, ehi := e.constTable(base)
		id := e.newVar()
		e.emit(ir.TLoad{Dst: id, Tab: tab, Idx: opnd(idx), Check: check})
		return dynScalar(base.ty.Elem, id, elo, ehi)
	}
	mem, elo, ehi := e.tableFor(base)
	id := e.newVar()
	e.emit(ir.Load{Dst: id, Arr: mem, Idx: opnd(idx), Check: check})
	return dynScalar(base.ty.Elem, id, elo, ehi)
}

// dynSelect reads the element chosen by a dynamic idx from an array of aggregates:
// the result is an aggregate whose scalar leaves are loads from per-leaf tables.
func (e *eng) dynSelect(vs []*V, ety *types.Type, idx *V) *V {
	switch ety.Kind {
	case types.KStruct:
		out := &V{k: kStruct, ty: ety, elems: make([]*V, len(ety.Fields))}
		for f := range ety.Fields {
			col := make([]*V, len(vs))
			for k, v := range vs {
				col[k] = v.elems[f]
			}
			out.elems[f] = e.dynSelect(col, ety.Fields[f].Ty, idx)
		}
		return out
	case types.KArray:
		out := &V{k: kArr, ty: ety, elems: make([]*V, ety.N)}
		for j := range out.elems {
			col := make([]*V, len(vs))
			for k, v := range vs {
				col[k] = e.elemOf(v, j)
			}
			out.elems[j] = e.dynSelect(col, ety.Elem, idx)
		}
		return out
	}
	if !ety.IsScalar() {
		e.fail("E0502", "dynamic indexing of an array of `%s` is unsupported", ety)
	}
	return e.indexValue(&V{k: kArr, ty: types.Array(ety, len(vs)), elems: vs}, idx)
}

// structArrayField evaluates `arr[i].f` for an array of structs. A constant index
// selects the element directly; a dynamic index lowers to a scalar load from a
// per-field table (struct-of-arrays view), so no aggregate ever needs run-time memory.
func (e *eng) structArrayField(en *env, ix *syntax.Index, f *syntax.Field) *V {
	var base *V
	if id, ok := ix.X.(*syntax.Ident); ok && en.lookup(id.Name) == nil {
		base = e.global(id.Name)
	}
	if base == nil {
		base = e.expr(en, ix.X)
	}
	idx := e.expr(en, ix.I)
	e.pos = ix.Pos
	if base.k == kSoA {
		return e.indexValue(base.elems[f.Index], idx)
	}
	if idx.k == kConst {
		e.checkIdx(idx, base.ty.N)
		return base.elems[idx.c.Int64()].elems[f.Index]
	}
	fty := base.ty.Elem.Fields[f.Index].Ty
	if !fty.IsScalar() {
		e.fail("E0502", "dynamic indexing of an array of structs supports scalar fields only")
	}
	view := &V{k: kArr, ty: types.Array(fty, base.ty.N), elems: make([]*V, len(base.elems))}
	for i, el := range base.elems {
		view.elems[i] = el.elems[f.Index]
	}
	return e.indexValue(view, idx)
}

// ---------------------------------------------------------------- calls

func (e *eng) call(en *env, n *syntax.Call) *V {
	switch n.Name {
	case "alloc":
		return e.allocCall(en, n)
	case "sizeof":
		return cInt(types.U64, big.NewInt(int64(n.Args[0].Type().Size())))
	case "alignof":
		return cInt(types.U64, big.NewInt(int64(n.Args[0].Type().Align())))
	case "bits_of":
		t := n.Args[0].Type()
		bits := int64(t.Bits)
		if t.Kind == types.KBool {
			bits = 1
		}
		return cInt(types.U64, big.NewInt(bits))
	case "max_of", "min_of":
		t := n.Args[0].Type()
		lo, hi := t.Range()
		if n.Name == "max_of" {
			return cInt(t, hi)
		}
		return cInt(t, lo)
	case "offsetof":
		st := n.Args[0].Type()
		return cInt(types.U64, big.NewInt(int64(st.Offset(st.FieldIndex(n.Args[1].(*syntax.Ident).Name)))))
	}
	args := make([]*V, len(n.Args))
	for i, a := range n.Args {
		args[i] = e.expr(en, a)
	}
	e.pos = n.Pos
	switch n.Name {
	case "io::println", "io::print":
		e.print(args[0], n.Name == "io::println")
		return unitV
	case "io::read_line":
		cap := int(args[0].c.Int64())
		b, ln := e.newBuf(cap), e.newVar()
		e.emit(ir.ReadLine{Buf: b, Len: ln, Cap: cap})
		return &V{k: kDyn, ty: types.Str(cap), v: ln, buf: b, lo: big.NewInt(0), hi: big.NewInt(int64(cap))}
	case "io::read_u32", "io::read_u64", "io::read_i32", "io::read_i64":
		t := n.Ty
		id := e.newVar()
		e.emit(ir.ReadInt{Dst: id, Ty: irTy(t)})
		return dynScalar(t, id, nil, nil)
	case "io::read_f32", "io::read_f64":
		t := fixTy(n.Ty)
		id := e.newVar()
		e.emit(ir.ReadFloat{Dst: id, F32: t.Bits == 32})
		return dynScalar(t, id, nil, nil)
	case "sqrt", "floor", "ceil", "trunc", "abs", "is_nan":
		if t := fixTy(n.Ty); n.Name == "abs" && t.IsInt() {
			// abs(x) = max(x, 0 - x); the subtraction traps exactly when x == min_of(T)
			neg := e.arith("-", cInt(t, big.NewInt(0)), args[0], t)
			return e.minmax(false, args[0], neg, t)
		}
		return e.floatUnary(n.Name, args[0])
	case "str":
		v := args[0]
		if v.ty.Kind == types.KBool {
			if v.k == kConst {
				e.stats.FoldedOps++
				if v.c.Sign() != 0 {
					return cStr("true")
				}
				return cStr("false")
			}
			buf, ln := e.newBuf(5), e.newVar()
			thenI := e.capture(func() {
				e.emit(ir.StrBuild{Buf: buf, Len: ln, Parts: []ir.StrPart{{Kind: 0, S: "true"}}})
			})
			elseI := e.capture(func() {
				e.emit(ir.StrBuild{Buf: buf, Len: ln, Parts: []ir.StrPart{{Kind: 0, S: "false"}}})
			})
			e.emit(ir.If{Cond: opnd(v), Then: thenI, Else: elseI})
			return &V{k: kDyn, ty: types.Str(5), v: ln, buf: buf, lo: big.NewInt(4), hi: big.NewInt(5)}
		}
		if v.k == kConst && v.ty.Kind == types.KFloat {
			e.stats.FoldedOps++
			return cStr(ir.FormatFloat(v.f))
		}
		if v.k == kConst {
			e.stats.FoldedOps++
			return cStr(v.c.String())
		}
		r := &V{k: kRope, parts: []part{{d: v}}}
		r.ty = types.Str(strCap(r))
		return r
	case "len":
		return e.lenOf(args[0])
	case "bytes":
		if args[0].k != kConst {
			e.failh("E0101", "pass a literal or a `const` string", "`bytes` needs a compile-time string")
		}
		return e.bytesV(args[0].s)
	case "embed":
		return e.bytesV(string(n.Embed))
	case "clamp", "require":
		return e.clampCall(n.Name == "require", args[0], args[1], args[2])
	case "min", "max":
		return e.minmax(n.Name == "min", args[0], args[1], fixTy(n.Ty))
	case "static_assert":
		if args[0].k != kConst {
			e.fail("E0101", "`static_assert` condition is not a compile-time constant")
		}
		if args[0].c.Sign() == 0 {
			if len(args) == 2 {
				e.fail("E0308", "static assertion failed: %s", args[1].s)
			}
			e.fail("E0308", "static assertion failed")
		}
		return unitV
	case "static_print":
		if args[0].k != kConst {
			e.failh("E0101", "only compile-time strings can be printed at build time", "`static_print` argument is not a compile-time string")
		}
		e.notes = append(e.notes, fmt.Sprintf("%s: %s", e.pos, args[0].s))
		return unitV
	}
	fn := e.info.Fns[n.Name]
	return e.callUser(fn, args)
}

func (e *eng) lenOf(v *V) *V {
	switch v.k {
	case kConst:
		if v.ty.Kind == types.KStr {
			return cInt(types.U32, big.NewInt(int64(len(v.s))))
		}
	case kArr, kMem, kStruct, kSoA:
		return cInt(types.U64, big.NewInt(int64(v.ty.N)))
	case kDyn:
		return &V{k: kDyn, ty: types.U32, v: v.v, lo: v.lo, hi: v.hi}
	case kRope:
		static := 0
		var acc *V
		for _, p := range v.parts {
			if p.d == nil {
				static += len(p.s)
				continue
			}
			if p.d.ty.Kind != types.KStr {
				return e.lenOf(e.materializeRope(v)) // numeric holes have no static length bound
			}
			h := &V{k: kDyn, ty: types.U32, v: p.d.v, lo: p.d.lo, hi: p.d.hi}
			if acc == nil {
				acc = h
			} else {
				acc = e.arith("+", acc, h, types.U32)
			}
		}
		if static > 0 {
			return e.arith("+", acc, cInt(types.U32, big.NewInt(int64(static))), types.U32)
		}
		return acc
	}
	e.fail("E0000", "internal: len of unsupported value")
	return nil
}

func (e *eng) clampCall(trap bool, x, lo, hi *V) *V {
	if lo.k != kConst || hi.k != kConst {
		e.fail("E0501", "`clamp`/`require` limits must be compile-time constants")
	}
	if lo.c.Cmp(hi.c) > 0 {
		e.fail("E0305", "empty range: %s > %s", lo.c, hi.c)
	}
	if x.k == kConst {
		if x.c.Cmp(lo.c) < 0 || x.c.Cmp(hi.c) > 0 {
			if trap {
				e.fail("E0308", "`require` fails at compile time: %s not in %s..=%s", x.c, lo.c, hi.c)
			}
			if x.c.Cmp(lo.c) < 0 {
				return cInt(x.ty, lo.c)
			}
			return cInt(x.ty, hi.c)
		}
		e.stats.FoldedOps++
		return x
	}
	if x.lo.Cmp(lo.c) >= 0 && x.hi.Cmp(hi.c) <= 0 {
		e.stats.FoldedOps++
		return x
	}
	if trap && (x.hi.Cmp(lo.c) < 0 || x.lo.Cmp(hi.c) > 0) {
		e.fail("E0308", "`require` always fails: value range %s..=%s is outside %s..=%s", x.lo, x.hi, lo.c, hi.c)
	}
	nlo := bmax(lo.c, bmin(x.lo, hi.c))
	nhi := bmin(hi.c, bmax(x.hi, lo.c))
	if trap {
		nlo, nhi = bmax(x.lo, lo.c), bmin(x.hi, hi.c)
	}
	id := e.newVar()
	e.emit(ir.Clamp{Dst: id, A: opnd(x), Lo: opnd(lo), Hi: opnd(hi), Ty: irTy(x.ty), TrapMode: trap})
	return dynScalar(x.ty, id, nlo, nhi)
}

var fuops = map[string]ir.FUOp{"sqrt": ir.FSqrt, "floor": ir.FFloor, "ceil": ir.FCeil, "trunc": ir.FTrunc, "abs": ir.FAbs, "is_nan": ir.FIsNan}

func (e *eng) floatUnary(name string, v *V) *V {
	op := fuops[name]
	f32 := v.ty.Bits == 32
	if v.k == kConst {
		e.stats.FoldedOps++
		r := ir.EvalFUn(op, vbits(v), f32)
		if op == ir.FIsNan {
			return cBool(r != 0)
		}
		return cFloat(v.ty, ir.ToFloat64(r, f32))
	}
	id := e.newVar()
	e.emit(ir.FUn{Dst: id, Op: op, A: opnd(v), F32: f32})
	if op == ir.FIsNan {
		return dynScalar(types.Bool, id, nil, nil)
	}
	return dynScalar(v.ty, id, nil, nil)
}

func (e *eng) minmax(isMin bool, a, b *V, ty *types.Type) *V {
	if ty.Kind == types.KFloat {
		fo := ir.FMax
		if isMin {
			fo = ir.FMin
		}
		f32 := ty.Bits == 32
		if a.k == kConst && b.k == kConst {
			e.stats.FoldedOps++
			return cFloat(ty, ir.ToFloat64(ir.EvalFBin(fo, vbits(a), vbits(b), f32), f32))
		}
		id := e.newVar()
		e.emit(ir.FBin{Dst: id, Op: fo, A: opnd(a), B: opnd(b), F32: f32})
		return dynScalar(ty, id, nil, nil)
	}
	alo, ahi := rng(a)
	blo, bhi := rng(b)
	if a.k == kConst && b.k == kConst {
		e.stats.FoldedOps++
		if (a.c.Cmp(b.c) <= 0) == isMin {
			return a
		}
		return b
	}
	var lo, hi *big.Int
	op := ir.Max
	if isMin {
		op = ir.Min
		lo, hi = bmin(alo, blo), bmin(ahi, bhi)
	} else {
		lo, hi = bmax(alo, blo), bmax(ahi, bhi)
	}
	id := e.newVar()
	e.emit(ir.Bin{Dst: id, Op: op, A: opnd(a), B: opnd(b), Ty: irTy(ty)})
	return dynScalar(ty, id, lo, hi)
}

func (e *eng) print(v *V, nl bool) {
	var parts []part
	switch v.k {
	case kConst:
		parts = []part{{s: v.s}}
	case kDyn:
		parts = []part{{d: v}}
	case kRope:
		parts = append(parts, v.parts...)
	}
	if nl {
		parts = append(parts, part{s: "\n"})
	}
	var merged []part
	for _, p := range parts {
		if p.d == nil && len(merged) > 0 && merged[len(merged)-1].d == nil {
			merged[len(merged)-1].s += p.s
		} else {
			merged = append(merged, p)
		}
	}
	for _, p := range merged {
		switch {
		case p.d == nil:
			if p.s != "" {
				e.emit(ir.WriteStr{S: p.s})
			}
		case p.d.ty.Kind == types.KStr:
			e.emit(ir.WriteBuf{Buf: p.d.buf, Len: ir.V(p.d.v)})
		case p.d.ty.Kind == types.KFloat:
			e.emit(ir.WriteFloat{A: opnd(p.d), F32: p.d.ty.Bits == 32})
		default:
			e.emit(ir.WriteDec{A: opnd(p.d), Ty: irTy(p.d.ty)})
		}
	}
}

func (e *eng) allocCall(en *env, n *syntax.Call) *V {
	name := n.Args[0].(*syntax.Ident).Name
	a := e.arenas[name]
	size := e.expr(en, n.Args[1])
	e.pos = n.Pos
	lo, hi := rng(size)
	_ = lo
	tlo, thi := size.ty.Range()
	_ = tlo
	if size.k == kDyn && hi.Cmp(thi) == 0 && thi.Cmp(big.NewInt(int64(a.size))) > 0 {
		e.failh("E0412", "bound it with `clamp(x, lo, hi)` or `require(x, lo, hi)` first",
			"allocation size in arena `%s` has no provable upper bound", name)
	}
	if !hi.IsInt64() || a.live+int(hi.Int64()) > a.size {
		e.failh("E0412", "shrink the allocation, wrap earlier ones in a `region`, or enlarge the arena",
			"arena `%s` budget exceeded: worst case %s live + %d requested > %d bytes",
			name, big.NewInt(int64(a.live)), hiInt(hi), a.size)
	}
	a.live += int(hi.Int64())
	if a.live > a.hw {
		a.hw = a.live
	}
	id := e.newVar()
	e.emit(ir.Alloc{Dst: id, Arena: a.id, Size: opnd(size)})
	return &V{k: kDyn, ty: types.Ptr, v: id, lo: big.NewInt(0), hi: big.NewInt(0)}
}

func hiInt(h *big.Int) int64 {
	if h.IsInt64() {
		return h.Int64()
	}
	return -1
}

func (e *eng) callUser(fn *syntax.FnDecl, args []*V) *V {
	for i, p := range fn.Params {
		a := args[i]
		if p.Lo != nil {
			lo, hi := rng(a)
			if lo.Cmp(p.Lo) < 0 || hi.Cmp(p.Hi) > 0 {
				e.failh("E0501", "bound the argument with `clamp`/`require`, or widen the parameter refinement",
					"argument %d of `%s` may be outside %s..=%s (value range %s..=%s)", i+1, fn.Name, p.Lo, p.Hi, lo, hi)
			}
		}
	}
	allC := true
	for _, a := range args {
		if !allConst(a) {
			allC = false
		}
	}
	if fn.Pure && allC {
		var kb strings.Builder
		kb.WriteString(fn.Name)
		for _, a := range args {
			kb.WriteString("|" + constKey(a))
		}
		if r, ok := e.memo[kb.String()]; ok {
			e.stats.MemoHits++
			return r
		}
		e.stats.PureCallsFolded++
		e.static++
		r := e.invoke(fn, args)
		e.static--
		e.memo[kb.String()] = r
		return r
	}
	return e.invoke(fn, args)
}

func (e *eng) invoke(fn *syntax.FnDecl, args []*V) *V {
	callPos := e.pos
	depth := e.active[fn.Name]
	if fn.BoundRec >= 0 && depth >= fn.BoundRec {
		e.failh("E0201", "raise bound(rec = N) or reduce the recursion depth",
			"recursion in `%s` exceeds bound(rec = %d)", fn.Name, fn.BoundRec)
	}
	e.active[fn.Name] = depth + 1
	if fn.Pure {
		e.pureCtx++
	}
	fr := &frame{fn: fn, retSlot: -1, flag: -1}
	en := newEnv()
	for i, p := range fn.Params {
		en.declare(p.Name, withCap(e.own(args[i]), p.Ty))
	}
	if fn.NestedRet {
		fr.flag = e.newVar()
		e.emit(ir.Mov{Dst: fr.flag, A: ir.K(0)})
		if fn.Ret.IsScalar() {
			fr.retSlot = e.newVar()
		} else if fn.Ret.Kind == types.KStr {
			z := *cStr("")
			z.capDecl = fn.Ret.Cap
			fr.retStr = e.strSlot(&z)
		} else if fn.Ret.Kind == types.KStruct || fn.Ret.Kind == types.KArray {
			fr.retAgg = e.zeroV(fn.Ret)
			e.prepSoA(fr.retAgg)
			e.materialize(&fr.retAgg)
		}
	}
	c := e.stmts(fr, en, fn.Body)
	_ = c
	e.active[fn.Name] = depth
	if fn.Pure {
		e.pureCtx--
	}
	e.pos = callPos
	if fr.dynRets > 0 {
		if fr.retAgg != nil {
			e.widen(&fr.retAgg)
			return fr.retAgg
		}
		if fr.retStr != nil {
			e.widen(&fr.retStr)
			return fr.retStr
		}
		if fr.retSlot < 0 {
			return unitV
		}
		return &V{k: kDyn, ty: fn.Ret, v: fr.retSlot, lo: fr.retLo, hi: fr.retHi}
	}
	if fr.retVal == nil {
		return unitV
	}
	return fr.retVal
}
