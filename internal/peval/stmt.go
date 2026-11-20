package peval

import (
	"math/big"
	"sort"

	"apus/internal/ir"
	"apus/internal/syntax"
	"apus/internal/types"
)

func sortedKeys(m map[string]bool) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func (e *eng) scoped(fr *frame, en *env, body []syntax.Stmt) ctl {
	en.push()
	c := e.stmts(fr, en, body)
	en.pop()
	return c
}

func (e *eng) stmt(fr *frame, en *env, st syntax.Stmt) ctl {
	e.tick(st.Position())
	switch s := st.(type) {
	case *syntax.Let:
		v := e.own(e.expr(en, s.Init))
		if s.Ty != nil && s.Ty.Kind == types.KStr && v.ty != nil && v.ty.Kind == types.KStr {
			c := *v
			c.capDecl = s.Ty.Cap
			v = &c
		}
		en.declare(s.Name, v)
	case *syntax.Assign:
		nv := e.expr(en, s.X)
		e.pos = s.Pos
		e.assign(en, s.Target, nv)
	case *syntax.ExprStmt:
		e.expr(en, s.X)
	case *syntax.Return:
		if s.X == nil {
			fr.retVal = nil
			return cRet
		}
		v := e.own(e.expr(en, s.X))
		if fr.dynRets > 0 && !fr.isMain {
			e.storeRet(fr, v)
		}
		fr.retVal = v
		return cRet
	case *syntax.If:
		return e.ifStmt(fr, en, s)
	case *syntax.Block:
		return e.scoped(fr, en, s.Body)
	case *syntax.Break:
		return cBreak
	case *syntax.Continue:
		return cContinue
	case *syntax.MatchStmt:
		return e.matchStmt(fr, en, s)
	case *syntax.While:
		return e.whileStmt(fr, en, s)
	case *syntax.For:
		return e.forStmt(fr, en, s)
	case *syntax.Region:
		a := e.arenas[s.Arena]
		slot := e.newVar()
		e.emit(ir.Mark{Slot: slot, Arena: a.id})
		saved := a.live
		c := e.scoped(fr, en, s.Body)
		e.emit(ir.Reset{Slot: slot, Arena: a.id})
		a.live = saved
		return c
	}
	return cNone
}

func (e *eng) storeRet(fr *frame, v *V) {
	if fr.retStr != nil {
		e.assignStr(fr.retStr, v)
		return
	}
	if fr.retAgg != nil {
		e.assignValue(&fr.retAgg, v)
		return
	}
	if fr.retSlot < 0 {
		return
	}
	if !v.ty.IsScalar() {
		e.fail("E0502", "dynamic return of a non-scalar value from an inlined function is unsupported in v0.2")
	}
	e.emit(ir.Mov{Dst: fr.retSlot, A: opnd(v)})
	lo, hi := rng(v)
	if fr.retLo == nil {
		fr.retLo, fr.retHi = lo, hi
	} else {
		fr.retLo, fr.retHi = bmin(fr.retLo, lo), bmax(fr.retHi, hi)
	}
}

// dynReturn handles `return` executed on a dynamically selected path.
func (e *eng) dynReturn(fr *frame, v *V) {
	if fr.isMain {
		if v == nil {
			e.emit(ir.Ret{})
		} else {
			e.emit(ir.Ret{A: opnd(v), Has: true})
		}
		return
	}
	if v != nil {
		e.storeRet(fr, v)
	}
	e.emit(ir.Mov{Dst: fr.flag, A: ir.K(1)})
	fr.dynRets++
}

func (e *eng) snapshotArenas() map[*arenaState]int {
	m := map[*arenaState]int{}
	for _, a := range e.order {
		m[a] = a.live
	}
	return m
}

func (e *eng) prepareLoop(en *env, body []syntax.Stmt) []string {
	names := map[string]bool{}
	assignedRoots(body, names)
	var ks []string
	for _, n := range sortedKeys(names) {
		b := en.lookup(n)
		if b == nil {
			continue
		}
		if names[n+"#"] {
			e.prepSoA(b.v)
		}
		e.materialize(&b.v)
		e.widen(&b.v)
		ks = append(ks, n)
	}
	return ks
}

func (e *eng) finishLoop(en *env, names []string) {
	for _, n := range names {
		e.widen(&en.lookup(n).v)
	}
}

func (e *eng) loopArenas(before map[*arenaState]int, trips *big.Int) {
	for _, a := range e.order {
		delta := a.live - before[a]
		if delta <= 0 {
			continue
		}
		total := new(big.Int).Mul(big.NewInt(int64(delta)), trips)
		total.Add(total, big.NewInt(int64(before[a])))
		if !total.IsInt64() || total.Int64() > int64(a.size) {
			e.failh("E0412", "wrap the loop body's allocations in a `region`, or enlarge the arena",
				"arena `%s` budget exceeded: a loop allocates %d bytes per iteration for up to %s iterations (limit %d)",
				a.name, delta, trips, a.size)
		}
		a.live = int(total.Int64())
		if a.live > a.hw {
			a.hw = a.live
		}
	}
}

// ---------------------------------------------------------------- assignment

func (e *eng) lref(en *env, x syntax.Expr) **V {
	switch n := x.(type) {
	case *syntax.Ident:
		return &en.lookup(n.Name).v
	case *syntax.Field:
		base := e.lref(en, n.X)
		return &(*base).elems[n.Index]
	case *syntax.Index:
		base := e.lref(en, n.X)
		arr := *base
		idx := e.expr(en, n.I)
		if idx.k == kConst && arr.k == kArr {
			e.checkIdx(idx, arr.ty.N)
			return &arr.elems[idx.c.Int64()]
		}
		e.fail("E0502", "dynamic or memory-backed index inside a nested assignment target is unsupported in v0.2")
	}
	return nil
}

func (e *eng) checkIdx(idx *V, n int) {
	if idx.c.Sign() < 0 || idx.c.Cmp(big.NewInt(int64(n))) >= 0 {
		e.failh("E0304", "valid indices are 0..", "index %s is out of range for array of length %d", idx.c, n)
	}
}

func (e *eng) memOf(v *V) int {
	if v.k == kMem {
		return v.mem
	}
	id, _, _ := e.tableFor(v)
	return id
}

func (e *eng) assignValue(ref **V, nv *V) {
	cur := *ref
	switch {
	case cur.k == kDyn && cur.slot && cur.ty.Kind == types.KStr:
		e.assignStr(cur, nv)
	case cur.k == kDyn && cur.slot && cur.ty.IsScalar():
		e.emit(ir.Mov{Dst: cur.v, A: opnd(nv)})
		lo, hi := rng(nv)
		*ref = &V{k: kDyn, ty: cur.ty, v: cur.v, lo: lo, hi: hi, slot: true}
	case cur.k == kSoA:
		for f := range cur.elems {
			if nv.k == kSoA {
				e.assignValue(&cur.elems[f], nv.elems[f])
				continue
			}
			elems := make([]*V, len(nv.elems))
			for k, el := range nv.elems {
				elems[k] = el.elems[f]
			}
			e.assignValue(&cur.elems[f], &V{k: kArr, ty: cur.elems[f].ty, elems: elems})
		}
	case cur.k == kArr && nv.k == kSoA:
		// keep cur tracked (it may sit inside a region): load each cell out of memory
		for k := range cur.elems {
			for f := range cur.elems[k].elems {
				e.assignValue(&cur.elems[k].elems[f], e.loadMem(nv.elems[f], k))
			}
		}
	case cur.k == kStruct:
		for i := range cur.elems {
			e.assignValue(&cur.elems[i], nv.elems[i])
		}
	case cur.k == kArr && nv.k != kMem:
		for i := range cur.elems {
			e.assignValue(&cur.elems[i], nv.elems[i])
		}
	case cur.k == kArr || cur.k == kMem:
		e.toMem(cur)
		src := e.memOf(nv)
		e.emit(ir.ArrCopy{Dst: cur.mem, Src: src})
		if nv.k == kMem {
			cur.elo, cur.ehi = bmin(cur.elo, nv.elo), bmax(cur.ehi, nv.ehi)
		} else {
			for _, el := range nv.elems {
				l, h := rng(el)
				cur.elo, cur.ehi = bmin(cur.elo, l), bmax(cur.ehi, h)
			}
		}
	default:
		nvo := e.own(nv)
		if cur.ty != nil && cur.ty.Kind == types.KStr && nvo.ty != nil && nvo.ty.Kind == types.KStr {
			// keep the variable's declared capacity so a later slot is sized for it
			c := *nvo
			c.capDecl = cur.capDecl
			if cur.ty.Cap > c.capDecl {
				c.capDecl = cur.ty.Cap
			}
			nvo = &c
		}
		*ref = nvo
	}
}

func (e *eng) assign(en *env, target syntax.Expr, nv *V) {
	if t, ok := target.(*syntax.Index); ok {
		if in, ok := t.X.(*syntax.Index); ok {
			if ot := in.X.Type(); ot != nil && ot.Kind == types.KArray && ot.Elem.Kind == types.KArray {
				e.assignNested(en, in, t, nv)
				return
			}
		}
		baseRef := e.lref(en, t.X)
		idx := e.expr(en, t.I)
		e.pos = t.Pos
		e.assignAt(baseRef, idx, nv)
		return
	}
	if f, ok := target.(*syntax.Field); ok {
		if ix, ok := f.X.(*syntax.Index); ok {
			if at := ix.X.Type(); at != nil && at.Kind == types.KArray && at.Elem.Kind == types.KStruct {
				e.assignStructArrayField(en, ix, f, nv)
				return
			}
		}
	}
	e.assignValue(e.lref(en, target), nv)
}

// maxScatter bounds the array length for dynamic-index writes into arrays of aggregates.
const maxScatter = 256

// assignStructArrayField performs `arr[i].f = nv` for an array of structs.
func (e *eng) assignStructArrayField(en *env, ix *syntax.Index, f *syntax.Field, nv *V) {
	baseRef := e.lref(en, ix.X)
	arr := *baseRef
	idx := e.expr(en, ix.I)
	e.pos = ix.Pos
	n := arr.ty.N
	if arr.k == kArr && flatStructArr(arr.ty) && idx.k != kConst {
		e.toSoA(arr) // top level only: regions pre-convert via prepSoA
	}
	if arr.k == kSoA {
		e.assignAt(&arr.elems[f.Index], idx, nv)
		return
	}
	if idx.k == kConst {
		e.checkIdx(idx, n)
		e.assignValue(&arr.elems[idx.c.Int64()].elems[f.Index], nv)
		return
	}
	refs := make([]**V, n)
	for k, el := range arr.elems {
		refs[k] = &el.elems[f.Index]
	}
	e.scatter(refs, arr.ty.Elem.Fields[f.Index].Ty, idx, nv)
}

// scatter writes nv into the element selected by a dynamic idx of an array whose
// elements are the values behind refs. Aggregates are split into scalar leaves;
// each leaf is gathered into a scratch array, stored with the usual bounds check
// and scattered back into every element's slot (O(N) per leaf).
func (e *eng) scatter(refs []**V, ety *types.Type, idx, nv *V) {
	n := len(refs)
	ilo, ihi := rng(idx)
	if ihi.Sign() < 0 || ilo.Cmp(big.NewInt(int64(n))) >= 0 {
		e.fail("E0304", "index is always out of range for array of length %d", n)
	}
	if n > maxScatter {
		e.fail("E0502", "dynamic-index assignment into an array of aggregates is limited to %d elements", maxScatter)
	}
	switch ety.Kind {
	case types.KStruct:
		for f := range ety.Fields {
			col := make([]**V, n)
			for k, r := range refs {
				col[k] = &(*r).elems[f]
			}
			e.scatter(col, ety.Fields[f].Ty, idx, nv.elems[f])
		}
		return
	case types.KArray:
		for j := 0; j < ety.N; j++ {
			col, flush := e.memCol(refs, j)
			e.scatter(col, ety.Elem, idx, e.elemOf(nv, j))
			flush()
		}
		return
	}
	if !ety.IsScalar() {
		e.fail("E0502", "dynamic-index assignment into an array of `%s` is unsupported", ety)
	}
	ops := make([]ir.Operand, n)
	for k, r := range refs {
		e.materialize(r)
		ops[k] = opnd(*r)
	}
	tmp := e.newArr(n, irTy(ety))
	e.emit(initInst(tmp, ops))
	check := ilo.Sign() < 0 || ihi.Cmp(big.NewInt(int64(n-1))) > 0
	e.emit(ir.Store{Arr: tmp, Idx: opnd(idx), A: opnd(nv), Check: check})
	nlo, nhi := rng(nv)
	for k, r := range refs {
		lo, hi := rng(*r)
		lo, hi = bmin(lo, nlo), bmax(hi, nhi)
		id := e.newVar()
		e.emit(ir.Load{Dst: id, Arr: tmp, Idx: opnd(cInt(types.U64, big.NewInt(int64(k))))})
		e.assignValue(r, dynScalar(ety, id, lo, hi))
	}
}

// assignAt performs base[idx] = nv for an already evaluated index.
func (e *eng) assignAt(baseRef **V, idx *V, nv *V) {
	arr := *baseRef
	n := arr.ty.N
	if arr.k == kArr && flatStructArr(arr.ty) && idx.k != kConst {
		e.toSoA(arr)
	}
	if arr.k == kSoA {
		for f := range arr.elems {
			e.assignAt(&arr.elems[f], idx, nv.elems[f])
		}
		return
	}
	if idx.k == kConst {
		e.checkIdx(idx, n)
		if arr.k == kArr {
			e.assignValue(&arr.elems[idx.c.Int64()], nv)
			return
		}
	}
	if arr.k == kArr && !arr.ty.Elem.IsScalar() {
		refs := make([]**V, n)
		for k := range arr.elems {
			refs[k] = &arr.elems[k]
		}
		e.scatter(refs, arr.ty.Elem, idx, nv)
		return
	}
	ilo, ihi := rng(idx)
	if ihi.Sign() < 0 || ilo.Cmp(big.NewInt(int64(n))) >= 0 {
		e.fail("E0304", "index is always out of range for array of length %d", n)
	}
	e.toMem(arr)
	check := ilo.Sign() < 0 || ihi.Cmp(big.NewInt(int64(n-1))) > 0
	e.emit(ir.Store{Arr: arr.mem, Idx: opnd(idx), A: opnd(nv), Check: check})
	l, h := rng(nv)
	arr.elo, arr.ehi = bmin(arr.elo, l), bmax(arr.ehi, h)
}

// assignNested performs outer[i][j] = nv. A constant i selects a row; a dynamic i
// with a constant j scatters along column j. Both dynamic is not supported yet.
func (e *eng) assignNested(en *env, in, t *syntax.Index, nv *V) {
	outerRef := e.lref(en, in.X)
	outer := *outerRef
	i := e.expr(en, in.I)
	j := e.expr(en, t.I)
	e.pos = t.Pos
	if i.k == kConst {
		e.checkIdx(i, outer.ty.N)
		if outer.k == kArr {
			e.assignAt(&outer.elems[i.c.Int64()], j, nv)
			return
		}
	}
	if outer.k != kArr {
		e.fail("E0502", "assignment through a memory-backed array of arrays is unsupported")
	}
	rows := make([]**V, len(outer.elems))
	for k := range outer.elems {
		rows[k] = &outer.elems[k]
	}
	if j.k == kConst {
		e.checkIdx(j, outer.ty.Elem.N)
		col, flush := e.memCol(rows, int(j.c.Int64()))
		e.scatter(col, outer.ty.Elem.Elem, i, nv)
		flush()
		return
	}
	// Both indices dynamic: check each against its own bound (so that j == M cannot
	// alias the next row), then scatter into the flattened leaves at i*M + j.
	N, M := outer.ty.N, outer.ty.Elem.N
	e.checkDyn(i, N)
	e.checkDyn(j, M)
	refs := make([]**V, N*M)
	var flushes []func()
	for c := 0; c < M; c++ {
		col, flush := e.memCol(rows, c)
		for k := 0; k < N; k++ {
			refs[k*M+c] = col[k]
		}
		flushes = append(flushes, flush)
	}
	i64, j64 := e.cast(i, types.U64), e.cast(j, types.U64)
	m := e.newVar()
	e.emit(ir.Bin{Dst: m, Op: ir.Mul, A: opnd(i64), B: ir.K(uint64(M)), Ty: irTy(types.U64)})
	f := e.newVar()
	e.emit(ir.Bin{Dst: f, Op: ir.Add, A: ir.V(m), B: opnd(j64), Ty: irTy(types.U64)})
	flat := dynScalar(types.U64, f, big.NewInt(0), big.NewInt(int64(N*M-1)))
	e.scatter(refs, outer.ty.Elem.Elem, flat, nv)
	for _, fl := range flushes {
		fl()
	}
}

// loadMem reads element j of a memory-backed array as a scalar value.
func (e *eng) loadMem(row *V, j int) *V {
	id := e.newVar()
	e.emit(ir.Load{Dst: id, Arr: row.mem, Idx: opnd(cInt(types.U64, big.NewInt(int64(j))))})
	return dynScalar(row.ty.Elem, id, row.elo, row.ehi)
}

// elemOf returns element j of an array value, whether tracked or memory-backed.
func (e *eng) elemOf(v *V, j int) *V {
	if v.k == kMem {
		return e.loadMem(v, j)
	}
	return v.elems[j]
}

// memCol returns refs to column j of a list of rows. A memory-backed row yields a
// holder loaded from memory; flush stores the (possibly updated) holders back.
func (e *eng) memCol(rows []**V, j int) ([]**V, func()) {
	col := make([]**V, len(rows))
	var flushes []func()
	for k, r := range rows {
		row := *r
		if row.k != kMem {
			col[k] = &row.elems[j]
			continue
		}
		h := new(*V)
		*h = e.loadMem(row, j)
		col[k] = h
		flushes = append(flushes, func() {
			e.emit(ir.Store{Arr: row.mem, Idx: ir.K(uint64(j)), A: opnd(*h)})
			l, hh := rng(*h)
			row.elo, row.ehi = bmin(row.elo, l), bmax(row.ehi, hh)
		})
	}
	return col, func() {
		for _, f := range flushes {
			f()
		}
	}
}

// checkDyn traps unless 0 <= v < n, using a checked load from a dummy array so the
// trap is the ordinary index-out-of-bounds one. Nothing is emitted when ranges prove it.
func (e *eng) checkDyn(v *V, n int) {
	lo, hi := rng(v)
	if lo.Sign() >= 0 && hi.Cmp(big.NewInt(int64(n-1))) <= 0 {
		return
	}
	if hi.Sign() < 0 || lo.Cmp(big.NewInt(int64(n))) >= 0 {
		e.fail("E0304", "index is always out of range for array of length %d", n)
	}
	da := e.newArr(n, irTy(types.U8))
	e.emit(ir.Load{Dst: e.newVar(), Arr: da, Idx: opnd(v), Check: true})
}
