// Package ref is a straightforward tree-walking reference interpreter for
// type-checked Apus programs. It performs no partial evaluation, so it serves
// as the oracle for differential testing of the compiler.
package ref

import (
	"bufio"
	"io"
	"math"
	"math/big"
	"strconv"

	"apus/internal/ir"
	"apus/internal/sema"
	"apus/internal/syntax"
	"apus/internal/types"
)

type Result struct {
	Ret      *big.Int // nil when main returns ()
	Trap     ir.TrapCode
	Calls    int
	Exceeded bool // the call budget was exhausted; other fields are not meaningful
}

type budgetExceeded struct{}

type rv struct {
	f     float64 // floats (f32 values are held rounded to float32)
	i     *big.Int
	s     []byte
	elems []*rv
}

type trap struct{ code ir.TrapCode }

type interp struct {
	info   *sema.Info
	in     *bufio.Reader
	out    *bufio.Writer
	scopes []map[string]*rv
	consts map[string]*rv
	calls  int
	limit  int
}

func (it *interp) trap(c ir.TrapCode) { panic(trap{c}) }

// Run executes main and returns its result. Traps are reported in Result.Trap.
func Run(info *sema.Info, in io.Reader, out io.Writer) Result {
	return RunLimited(info, in, out, 0)
}

// RunLimited is like Run but gives up after maxCalls function calls (0 = unlimited).
func RunLimited(info *sema.Info, in io.Reader, out io.Writer, maxCalls int) (res Result) {
	it := &interp{info: info, in: bufio.NewReader(in), out: bufio.NewWriter(out), consts: map[string]*rv{}, limit: maxCalls}
	defer func() {
		if r := recover(); r != nil {
			switch t := r.(type) {
			case trap:
				res.Trap = t.code
			case budgetExceeded:
				res.Exceeded = true
			default:
				panic(r)
			}
		}
		res.Calls = it.calls
		it.out.Flush()
	}()
	main := info.Fns["main"]
	v, _ := it.callFn(main, nil)
	if v != nil && main.Ret.Kind == types.KInt {
		res.Ret = v.i
	}
	return
}

func (it *interp) push()                   { it.scopes = append(it.scopes, map[string]*rv{}) }
func (it *interp) pop()                    { it.scopes = it.scopes[:len(it.scopes)-1] }
func (it *interp) declare(n string, v *rv) { it.scopes[len(it.scopes)-1][n] = v }
func (it *interp) lookup(n string) (*rv, int) {
	for i := len(it.scopes) - 1; i >= 0; i-- {
		if v, ok := it.scopes[i][n]; ok {
			return v, i
		}
	}
	return nil, -1
}

func cp(v *rv) *rv {
	if v == nil {
		return nil
	}
	n := &rv{i: v.i, s: v.s, f: v.f}
	if v.elems != nil {
		n.elems = make([]*rv, len(v.elems))
		for i, e := range v.elems {
			n.elems[i] = cp(e)
		}
	}
	return n
}

func bi(n int64) *rv { return &rv{i: big.NewInt(n)} }
func bb(b bool) *rv {
	if b {
		return bi(1)
	}
	return bi(0)
}

func (it *interp) callFn(fn *syntax.FnDecl, args []*rv) (*rv, bool) {
	it.calls++
	if it.limit > 0 && it.calls > it.limit {
		panic(budgetExceeded{})
	}
	saved := it.scopes
	it.scopes = []map[string]*rv{{}}
	for i, p := range fn.Params {
		it.declare(p.Name, cp(args[i]))
	}
	v, _ := it.block(fn.Body)
	it.scopes = saved
	return v, true
}

const (
	rcNone = iota
	rcRet
	rcBreak
	rcCont
)

func (it *interp) block(b []syntax.Stmt) (*rv, int) {
	it.push()
	for _, s := range b {
		if v, c := it.stmt(s); c != rcNone {
			it.pop()
			return v, c
		}
	}
	it.pop()
	return nil, rcNone
}

func (it *interp) pickArm(v *big.Int, arms []*syntax.Arm) *syntax.Arm {
	for _, a := range arms {
		for _, p := range a.Pats {
			if p.Wild || (v.Cmp(p.Lo) >= 0 && v.Cmp(p.Hi) <= 0) {
				return a
			}
		}
	}
	panic("ref: non-exhaustive match")
}

func (it *interp) stmt(st syntax.Stmt) (*rv, int) {
	switch s := st.(type) {
	case *syntax.Let:
		it.declare(s.Name, cp(it.eval(s.Init)))
	case *syntax.Assign:
		v := cp(it.eval(s.X))
		it.assign(s.Target, v)
	case *syntax.ExprStmt:
		it.eval(s.X)
	case *syntax.Return:
		if s.X == nil {
			return nil, rcRet
		}
		return it.eval(s.X), rcRet
	case *syntax.Break:
		return nil, rcBreak
	case *syntax.Continue:
		return nil, rcCont
	case *syntax.If:
		if it.eval(s.Cond).i.Sign() != 0 {
			return it.block(s.Then)
		}
		return it.block(s.Else)
	case *syntax.While:
		count := 0
		for it.eval(s.Cond).i.Sign() != 0 {
			if count >= s.Bound {
				it.trap(ir.TrapBound)
			}
			v, c := it.block(s.Body)
			if c == rcRet {
				return v, c
			}
			if c == rcBreak {
				break
			}
			count++
		}
	case *syntax.For:
		lo, hi := it.eval(s.Lo).i, it.eval(s.Hi).i
		for i := new(big.Int).Set(lo); i.Cmp(hi) < 0; i.Add(i, big.NewInt(1)) {
			it.push()
			it.declare(s.Var, &rv{i: new(big.Int).Set(i)})
			v, c := it.block(s.Body)
			it.pop()
			if c == rcRet {
				return v, c
			}
			if c == rcBreak {
				break
			}
		}
	case *syntax.Region:
		return it.block(s.Body)
	case *syntax.Block:
		return it.block(s.Body)
	case *syntax.MatchStmt:
		return it.block(it.pickArm(it.eval(s.X).i, s.Arms).Body)
	}
	return nil, rcNone
}

func (it *interp) lv(x syntax.Expr) *rv {
	switch n := x.(type) {
	case *syntax.Ident:
		v, _ := it.lookup(n.Name)
		return v
	case *syntax.Index:
		base := it.lv(n.X)
		return base.elems[it.idx(n, base)]
	case *syntax.Field:
		return it.lv(n.X).elems[n.Index]
	}
	panic("ref: bad lvalue")
}

func (it *interp) idx(n *syntax.Index, base *rv) int {
	i := it.eval(n.I).i
	if i.Sign() < 0 || i.Cmp(big.NewInt(int64(len(base.elems)))) >= 0 {
		it.trap(ir.TrapIndex)
	}
	return int(i.Int64())
}

func (it *interp) assign(target syntax.Expr, v *rv) {
	switch t := target.(type) {
	case *syntax.Ident:
		_, depth := it.lookup(t.Name)
		it.scopes[depth][t.Name] = v
	case *syntax.Index:
		base := it.lv(t.X)
		base.elems[it.idx(t, base)] = v
	case *syntax.Field:
		it.lv(t.X).elems[t.Index] = v
	}
}

func inRange(x *big.Int, t *types.Type) bool {
	lo, hi := t.Range()
	return x.Cmp(lo) >= 0 && x.Cmp(hi) <= 0
}

func wrap(r *big.Int, t *types.Type) *big.Int {
	mask := new(big.Int).Lsh(big.NewInt(1), uint(t.Bits))
	mask.Sub(mask, big.NewInt(1))
	out := new(big.Int).And(r, mask)
	if t.Signed && out.Bit(t.Bits-1) == 1 {
		out.Sub(out, new(big.Int).Lsh(big.NewInt(1), uint(t.Bits)))
	}
	return out
}

func ty(t *types.Type) *types.Type {
	if t == nil || t.Kind == types.KUntyped {
		return types.I32
	}
	return t
}

func (it *interp) eval(x syntax.Expr) *rv {
	switch n := x.(type) {
	case *syntax.IntLit:
		if n.Ty != nil && n.Ty.Kind == types.KFloat {
			f, _ := new(big.Float).SetInt(n.V).Float64()
			return &rv{f: roundF(f, n.Ty.Bits)}
		}
		return &rv{i: n.V}
	case *syntax.FloatLit:
		bits := 64
		if n.Ty != nil && n.Ty.Kind == types.KFloat {
			bits = n.Ty.Bits
		}
		f, _ := strconv.ParseFloat(n.Text, bits)
		return &rv{f: f}
	case *syntax.StrLit:
		return &rv{s: []byte(n.V)}
	case *syntax.BoolLit:
		return bb(n.V)
	case *syntax.Ident:
		if n.EnumVal != nil {
			return &rv{i: n.EnumVal}
		}
		if v, _ := it.lookup(n.Name); v != nil {
			return v
		}
		if v, ok := it.consts[n.Name]; ok {
			return v
		}
		saved := it.scopes
		it.scopes = []map[string]*rv{{}}
		v := it.eval(it.info.Consts[n.Name].X)
		it.scopes = saved
		it.consts[n.Name] = v
		return v
	case *syntax.Static:
		return it.eval(n.X)
	case *syntax.Unary:
		v := it.eval(n.X)
		if n.Op == "!" {
			return bb(v.i.Sign() == 0)
		}
		if n.Ty != nil && n.Ty.Kind == types.KFloat {
			return &rv{f: -v.f}
		}
		r := new(big.Int).Neg(v.i)
		if !inRange(r, ty(n.Ty)) {
			it.trap(ir.TrapOverflow)
		}
		return &rv{i: r}
	case *syntax.Binary:
		return it.binary(n)
	case *syntax.Cast:
		v := it.eval(n.X)
		if from := n.X.Type(); (from != nil && from.Kind == types.KFloat) || (n.Ty != nil && n.Ty.Kind == types.KFloat) {
			return it.castFloat(v, from, n.Ty)
		}
		if !inRange(v.i, ty(n.Ty)) {
			it.trap(ir.TrapOverflow)
		}
		return v
	case *syntax.Index:
		base := it.eval(n.X)
		if n.X.Type().Kind == types.KStr {
			i := it.eval(n.I).i
			if i.Sign() < 0 || i.Cmp(big.NewInt(int64(len(base.s)))) >= 0 {
				it.trap(ir.TrapIndex)
			}
			return bi(int64(base.s[i.Int64()]))
		}
		return base.elems[it.idx(n, base)]
	case *syntax.MatchExpr:
		return it.eval(it.pickArm(it.eval(n.X).i, n.Arms).Val)
	case *syntax.Field:
		return it.eval(n.X).elems[n.Index]
	case *syntax.StructLit:
		out := &rv{elems: make([]*rv, len(n.Ty.Fields))}
		for _, fi := range n.Fields {
			out.elems[n.Ty.FieldIndex(fi.Name)] = cp(it.eval(fi.X))
		}
		return out
	case *syntax.ArrayLit:
		out := &rv{elems: make([]*rv, len(n.Elems))}
		for i, e := range n.Elems {
			out.elems[i] = cp(it.eval(e))
		}
		return out
	case *syntax.ArrayRep:
		v := it.eval(n.Val)
		out := &rv{elems: make([]*rv, n.Len)}
		for i := range out.elems {
			out.elems[i] = cp(v)
		}
		return out
	case *syntax.Call:
		return it.call(n)
	}
	panic("ref: unhandled expression")
}

func (it *interp) binary(n *syntax.Binary) *rv {
	switch n.Op {
	case "&&":
		if it.eval(n.L).i.Sign() == 0 {
			return bi(0)
		}
		return it.eval(n.R)
	case "||":
		if it.eval(n.L).i.Sign() != 0 {
			return bi(1)
		}
		return it.eval(n.R)
	case "++":
		a, b := it.eval(n.L), it.eval(n.R)
		return &rv{s: append(append([]byte{}, a.s...), b.s...)}
	}
	a, b := it.eval(n.L), it.eval(n.R)
	if n.L.Type().Kind == types.KStr {
		eq := string(a.s) == string(b.s)
		return bb(eq == (n.Op == "=="))
	}
	if lt := n.L.Type(); lt != nil && lt.Kind == types.KFloat {
		return fbinary(n.Op, a.f, b.f, lt.Bits)
	}
	switch n.Op {
	case "==", "!=", "<", "<=", ">", ">=":
		c := a.i.Cmp(b.i)
		switch n.Op {
		case "==":
			return bb(c == 0)
		case "!=":
			return bb(c != 0)
		case "<":
			return bb(c < 0)
		case "<=":
			return bb(c <= 0)
		case ">":
			return bb(c > 0)
		}
		return bb(c >= 0)
	}
	t := ty(n.Ty)
	x, y := a.i, b.i
	r := new(big.Int)
	switch n.Op {
	case "+":
		r.Add(x, y)
	case "-":
		r.Sub(x, y)
	case "*":
		r.Mul(x, y)
	case "/", "%":
		if y.Sign() == 0 {
			it.trap(ir.TrapDivZero)
		}
		if n.Op == "/" {
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
		if y.Sign() < 0 || y.Cmp(big.NewInt(int64(t.Bits))) >= 0 {
			it.trap(ir.TrapShift)
		}
		if n.Op == "<<" {
			return &rv{i: wrap(r.Lsh(x, uint(y.Uint64())), t)}
		}
		r.Rsh(x, uint(y.Uint64()))
	}
	if !inRange(r, t) {
		it.trap(ir.TrapOverflow)
	}
	return &rv{i: r}
}

func (it *interp) call(n *syntax.Call) *rv {
	switch n.Name {
	case "alloc":
		it.eval(n.Args[1])
		return bi(0)
	case "sizeof":
		return bi(int64(n.Args[0].Type().Size()))
	case "alignof":
		return bi(int64(n.Args[0].Type().Align()))
	case "bits_of":
		if n.Args[0].Type().Kind == types.KBool {
			return bi(1)
		}
		return bi(int64(n.Args[0].Type().Bits))
	case "max_of":
		_, hi := n.Args[0].Type().Range()
		return &rv{i: hi}
	case "min_of":
		lo, _ := n.Args[0].Type().Range()
		return &rv{i: lo}
	case "offsetof":
		st := n.Args[0].Type()
		return bi(int64(st.Offset(st.FieldIndex(n.Args[1].(*syntax.Ident).Name))))
	case "embed":
		return byteArray(n.Embed)
	}
	args := make([]*rv, len(n.Args))
	for i, a := range n.Args {
		args[i] = it.eval(a)
	}
	switch n.Name {
	case "io::println", "io::print":
		it.out.Write(args[0].s)
		if n.Name == "io::println" {
			it.out.WriteByte('\n')
		}
		return nil
	case "io::read_line":
		cap := int(args[0].i.Int64())
		var buf []byte
		for {
			c, err := it.in.ReadByte()
			if err != nil || c == '\n' {
				break
			}
			if len(buf) < cap {
				buf = append(buf, c)
			}
		}
		return &rv{s: buf}
	case "io::read_u32", "io::read_u64", "io::read_i32", "io::read_i64":
		t := n.Ty
		v, ok := ir.ReadIntFrom(it.in, ir.IntTy{Bits: t.Bits, Signed: t.Signed})
		if !ok {
			it.trap(ir.TrapInput)
		}
		if t.Signed {
			return &rv{i: big.NewInt(int64(v))}
		}
		return &rv{i: new(big.Int).SetUint64(v)}
	case "str":
		if t := n.Args[0].Type(); t != nil && t.Kind == types.KBool {
			if args[0].i.Sign() != 0 {
				return &rv{s: []byte("true")}
			}
			return &rv{s: []byte("false")}
		}
		if t := n.Args[0].Type(); t != nil && t.Kind == types.KFloat {
			return &rv{s: []byte(ir.FormatFloat(args[0].f))}
		}
		return &rv{s: []byte(args[0].i.String())}
	case "io::read_f32", "io::read_f64":
		f32 := n.Ty.Bits == 32
		bits, ok := ir.ReadFloatFrom(it.in, f32)
		if !ok {
			it.trap(ir.TrapInput)
		}
		return &rv{f: ir.ToFloat64(bits, f32)}
	case "sqrt", "floor", "ceil", "trunc", "abs", "is_nan":
		if n.Name == "abs" && n.Args[0].Type().IsInt() {
			r := new(big.Int).Abs(args[0].i)
			if _, hi := n.Args[0].Type().Range(); r.Cmp(hi) > 0 {
				it.trap(ir.TrapOverflow)
			}
			return &rv{i: r}
		}
		x := args[0].f
		switch n.Name {
		case "sqrt":
			return &rv{f: roundF(math.Sqrt(x), n.Args[0].Type().Bits)}
		case "floor":
			return &rv{f: math.Floor(x)}
		case "ceil":
			return &rv{f: math.Ceil(x)}
		case "trunc":
			return &rv{f: math.Trunc(x)}
		case "abs":
			return &rv{f: math.Abs(x)}
		}
		return bb(x != x)
	case "bytes":
		return byteArray(args[0].s)
	case "len":
		if args[0].elems != nil {
			return bi(int64(len(args[0].elems)))
		}
		return bi(int64(len(args[0].s)))
	case "clamp", "require":
		v, lo, hi := args[0].i, args[1].i, args[2].i
		if v.Cmp(lo) < 0 || v.Cmp(hi) > 0 {
			if n.Name == "require" {
				it.trap(ir.TrapRequire)
			}
			if v.Cmp(lo) < 0 {
				return &rv{i: lo}
			}
			return &rv{i: hi}
		}
		return args[0]
	case "min":
		if t := n.Args[0].Type(); t != nil && t.Kind == types.KFloat {
			if args[1].f < args[0].f {
				return args[1]
			}
			return args[0]
		}
		if args[0].i.Cmp(args[1].i) <= 0 {
			return args[0]
		}
		return args[1]
	case "max":
		if t := n.Args[0].Type(); t != nil && t.Kind == types.KFloat {
			if args[1].f > args[0].f {
				return args[1]
			}
			return args[0]
		}
		if args[0].i.Cmp(args[1].i) >= 0 {
			return args[0]
		}
		return args[1]
	case "static_assert":
		if args[0].i.Sign() == 0 {
			it.trap(ir.TrapRequire)
		}
		return nil
	}
	v, _ := it.callFn(it.info.Fns[n.Name], args)
	return v
}

var _ = strconv.Itoa

func byteArray(b []byte) *rv {
	out := &rv{elems: make([]*rv, len(b))}
	for i, c := range b {
		out.elems[i] = bi(int64(c))
	}
	return out
}

func roundF(f float64, bits int) float64 {
	if bits == 32 {
		return float64(float32(f))
	}
	return f
}

// fbinary implements float arithmetic/comparison with native Go operators at the given width.
func fbinary(op string, a, b float64, bits int) *rv {
	if bits == 32 {
		x, y := float32(a), float32(b)
		switch op {
		case "+":
			return &rv{f: float64(x + y)}
		case "-":
			return &rv{f: float64(x - y)}
		case "*":
			return &rv{f: float64(x * y)}
		case "/":
			return &rv{f: float64(x / y)}
		case "==":
			return bb(x == y)
		case "!=":
			return bb(x != y)
		case "<":
			return bb(x < y)
		case "<=":
			return bb(x <= y)
		case ">":
			return bb(x > y)
		}
		return bb(x >= y)
	}
	switch op {
	case "+":
		return &rv{f: a + b}
	case "-":
		return &rv{f: a - b}
	case "*":
		return &rv{f: a * b}
	case "/":
		return &rv{f: a / b}
	case "==":
		return bb(a == b)
	case "!=":
		return bb(a != b)
	case "<":
		return bb(a < b)
	case "<=":
		return bb(a <= b)
	case ">":
		return bb(a > b)
	}
	return bb(a >= b)
}

// castFloat converts to/from floats using big.Float so rounding is independent of the IR helpers.
func (it *interp) castFloat(v *rv, from, to *types.Type) *rv {
	switch {
	case from.Kind == types.KFloat && to.Kind == types.KFloat:
		return &rv{f: roundF(v.f, to.Bits)}
	case from.Kind == types.KFloat:
		t := math.Trunc(v.f)
		if t != t || math.IsInf(t, 0) {
			it.trap(ir.TrapOverflow)
		}
		bi, _ := big.NewFloat(t).Int(nil)
		if !inRange(bi, to) {
			it.trap(ir.TrapOverflow)
		}
		return &rv{i: bi}
	}
	bf := new(big.Float).SetInt(v.i)
	if to.Bits == 32 {
		f, _ := bf.Float32()
		return &rv{f: float64(f)}
	}
	f, _ := bf.Float64()
	return &rv{f: f}
}
