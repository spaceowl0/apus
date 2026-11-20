package peval

import (
	"fmt"
	"math/big"

	"apus/internal/diag"
	"apus/internal/ir"
	"apus/internal/sema"
	"apus/internal/syntax"
	"apus/internal/types"
)

type Options struct {
	Fuel        int // maximum abstract evaluation steps
	UnrollLimit int // maximum static trip count that is fully unrolled
	// Defines overrides typed integer/bool constants (see sema.Options.Defines).
	Defines map[string]*big.Int
}

func DefaultOptions() Options { return Options{Fuel: 100_000_000, UnrollLimit: 256} }

type Stats struct {
	BranchesRemoved int
	PureCallsFolded int
	MemoHits        int
	UnrolledIters   int
	DynLoops        int
	FoldedOps       int
	FuelUsed        int
	ResidualInsts   int
	InstsBeforeOpt  int
}

type ArenaReport struct {
	Name      string
	Size      int
	HighWater int
}

type Result struct {
	Prog     *ir.Program
	Stats    Stats
	Arenas   []ArenaReport
	Warnings []string
	Notes    []string // output of static_print
}

type binding struct{ v *V }

type env struct{ scopes []map[string]*binding }

func newEnv() *env {
	return &env{scopes: []map[string]*binding{{}}}
}
func (e *env) push() { e.scopes = append(e.scopes, map[string]*binding{}) }
func (e *env) pop()  { e.scopes = e.scopes[:len(e.scopes)-1] }
func (e *env) lookup(n string) *binding {
	for i := len(e.scopes) - 1; i >= 0; i-- {
		if b, ok := e.scopes[i][n]; ok {
			return b
		}
	}
	return nil
}
func (e *env) declare(n string, v *V) { e.scopes[len(e.scopes)-1][n] = &binding{v} }

type ctl int

const (
	cNone ctl = iota
	cRet
	cDone
	cBreak
	cContinue
)

// loopCtx tracks the break/continue flags of the innermost enclosing loop.
type loopCtx struct {
	brk, cont         int // flag variables (valid when the body contains break/continue)
	hasBrk, hasCont   bool
	breaks, continues int // number of dynamic break/continue events so far
}

type frame struct {
	fn        *syntax.FnDecl
	isMain    bool
	retVal    *V
	retSlot   int
	retAgg    *V // slot-backed struct/array result of dynamic returns
	retStr    *V // slot-backed string result of dynamic returns
	flag      int
	dynRets   int
	loopDepth int
	loops     []*loopCtx
	retLo     *big.Int
	retHi     *big.Int
}

type arenaState struct {
	id, size, live, hw int
	name               string
}

type eng struct {
	info    *sema.Info
	opt     Options
	fuel    int
	buf     []ir.Inst
	pro     []ir.Inst // hoisted constant-table initialisation
	nvars   int
	arrs    []ir.ArrInfo
	bufs    []int
	arenas  map[string]*arenaState
	order   []*arenaState
	memo    map[string]*V
	active  map[string]int
	gconst  map[string]*V
	gbusy   map[string]bool
	tables  map[string]int // constant table key -> table id
	tabList []ir.Table
	pos     diag.Pos
	pureCtx int
	static  int // >0 while evaluating a pure call with constant arguments: loops never stay residual
	stats   Stats
	warns   []string
	notes   []string
	regions []int // stack of emission-region ids (one per nested capture)
	nextReg int
}

// inRegion reports whether code emitted in region id is guaranteed to have run
// before code emitted now (id is the current region or an enclosing one).
func (e *eng) inRegion(id int) bool {
	for _, r := range e.regions {
		if r == id {
			return true
		}
	}
	return false
}

func (e *eng) region() int {
	if len(e.regions) == 0 {
		return 0
	}
	return e.regions[len(e.regions)-1]
}

func (e *eng) fail(code string, f string, a ...any) {
	diag.Fail(diag.Errf(code, e.pos, f, a...))
}
func (e *eng) failh(code, help, f string, a ...any) {
	diag.Fail(diag.Errf(code, e.pos, f, a...).WithHelp(help))
}

func (e *eng) tick(p diag.Pos) {
	e.pos = p
	e.fuel--
	if e.fuel < 0 {
		e.failh("E0103", "raise the fuel limit, or reduce static work (memoized pure functions are cheap)",
			"compile-time fuel exhausted after %d steps", e.opt.Fuel)
	}
}

func (e *eng) emit(in ir.Inst) { e.buf = append(e.buf, in) }

// alwaysTraps handles an operation whose every outcome is a trap: it emits an
// unconditional trap (preserving run-time behaviour on this path), records a
// warning and returns a placeholder value of type t.
func (e *eng) alwaysTraps(t *types.Type, what string) *V {
	e.warns = append(e.warns, fmt.Sprintf("%s: %s always traps if this path is executed", e.pos, what))
	e.emit(ir.Trap{Code: ir.TrapOverflow})
	return dynScalar(t, e.newVar(), nil, nil)
}

func (e *eng) capture(f func()) []ir.Inst {
	saved := e.buf
	e.buf = nil
	e.nextReg++
	e.regions = append(e.regions, e.nextReg)
	f()
	e.regions = e.regions[:len(e.regions)-1]
	r := e.buf
	e.buf = saved
	return r
}

func (e *eng) newVar() int {
	e.nvars++
	return e.nvars - 1
}
func (e *eng) newArr(n int, t ir.IntTy) int {
	e.arrs = append(e.arrs, ir.ArrInfo{N: n, Ty: t})
	return len(e.arrs) - 1
}
func (e *eng) newBuf(cap int) int {
	e.bufs = append(e.bufs, cap)
	return len(e.bufs) - 1
}

// Compile partially evaluates the program's main function into residual IR.
func Compile(info *sema.Info, opt Options) (res *Result, err *diag.Error) {
	e := &eng{
		info: info, opt: opt, fuel: opt.Fuel, arenas: map[string]*arenaState{},
		memo: map[string]*V{}, active: map[string]int{}, gconst: map[string]*V{},
		gbusy: map[string]bool{}, tables: map[string]int{},
	}
	for i, a := range info.Prog.Arenas {
		st := &arenaState{id: i, size: a.Size, name: a.Name}
		e.arenas[a.Name] = st
		e.order = append(e.order, st)
	}
	err = diag.Try(func() {
		main := info.Fns["main"]
		fr := &frame{fn: main, isMain: true, retSlot: -1, flag: -1}
		en := newEnv()
		c := e.stmts(fr, en, main.Body)
		switch c {
		case cNone:
			e.emit(ir.Ret{})
		case cRet:
			if fr.retVal == nil {
				e.emit(ir.Ret{})
			} else {
				e.emit(ir.Ret{A: opnd(fr.retVal), Has: true})
			}
		}
	})
	if err != nil {
		return nil, err
	}
	p := &ir.Program{NVars: e.nvars, Arrs: e.arrs, Bufs: e.bufs, Tables: e.tabList}
	for _, a := range e.order {
		p.Arenas = append(p.Arenas, ir.Arena{Name: a.name, Size: a.size})
	}
	if rt := info.Fns["main"].Ret; rt.Kind == types.KInt {
		p.HasRet, p.RetTy = true, irTy(rt)
	}
	p.Body = append(append([]ir.Inst{}, e.pro...), e.buf...)
	e.stats.InstsBeforeOpt = ir.CountInsts(p.Body)
	ir.Optimize(p)
	e.stats.ResidualInsts = ir.CountInsts(p.Body)
	e.stats.FuelUsed = opt.Fuel - e.fuel
	res = &Result{Prog: p, Stats: e.stats, Warnings: e.warns, Notes: e.notes}
	for _, a := range e.order {
		res.Arenas = append(res.Arenas, ArenaReport{a.name, a.size, a.hw})
	}
	return res, nil
}

// ---------------------------------------------------------------- ownership

// own returns a value safe to bind to a new variable: slot-backed scalars are
// snapshotted and aggregates are deep-copied.
func (e *eng) own(v *V) *V {
	switch v.k {
	case kDyn:
		if v.ty.Kind == types.KStr && v.slot {
			return e.copyStr(v)
		}
		if v.ty.IsScalar() && v.slot {
			id := e.newVar()
			e.emit(ir.Mov{Dst: id, A: ir.V(v.v)})
			return &V{k: kDyn, ty: v.ty, v: id, lo: v.lo, hi: v.hi}
		}
		return v
	case kRope:
		np := make([]part, len(v.parts))
		for i, p := range v.parts {
			np[i] = part{s: p.s}
			if p.d != nil {
				np[i].d = e.own(p.d)
			}
		}
		return &V{k: kRope, ty: v.ty, parts: np}
	case kArr, kStruct, kSoA:
		n := &V{k: v.k, ty: v.ty, elems: make([]*V, len(v.elems))}
		for i, el := range v.elems {
			n.elems[i] = e.own(el)
		}
		return n
	case kMem:
		id := e.newArr(v.ty.N, irTy(v.ty.Elem))
		e.emit(ir.ArrCopy{Dst: id, Src: v.mem})
		return &V{k: kMem, ty: v.ty, mem: id, elo: v.elo, ehi: v.ehi}
	}
	return v
}

// ------------------------------------------------------------ materialization

func assignedRoots(b []syntax.Stmt, out map[string]bool) {
	for _, s := range b {
		switch s := s.(type) {
		case *syntax.Assign:
			r := rootName(s.Target)
			out[r] = true
			if hasDynIndex(s.Target) {
				out[r+"#"] = true // marker: this root is written through a run-time index
			}
		case *syntax.If:
			assignedRoots(s.Then, out)
			assignedRoots(s.Else, out)
		case *syntax.While:
			assignedRoots(s.Body, out)
		case *syntax.For:
			assignedRoots(s.Body, out)
		case *syntax.Region:
			assignedRoots(s.Body, out)
		case *syntax.Block:
			assignedRoots(s.Body, out)
		case *syntax.MatchStmt:
			for _, a := range s.Arms {
				assignedRoots(a.Body, out)
			}
		}
	}
}

// hasDynIndex reports whether an assignment target indexes with anything but a literal.
func hasDynIndex(x syntax.Expr) bool {
	switch n := x.(type) {
	case *syntax.Index:
		if _, ok := n.I.(*syntax.IntLit); !ok {
			return true
		}
		return hasDynIndex(n.X)
	case *syntax.Field:
		return hasDynIndex(n.X)
	}
	return false
}

func rootName(x syntax.Expr) string {
	switch n := x.(type) {
	case *syntax.Ident:
		return n.Name
	case *syntax.Index:
		return rootName(n.X)
	case *syntax.Field:
		return rootName(n.X)
	}
	return ""
}

// materialize turns *ref into slot-backed storage so assignments inside dynamic
// control flow can be expressed as residual instructions.
func (e *eng) materialize(ref **V) {
	v := *ref
	switch v.k {
	case kConst, kDyn:
		if v.ty.Kind == types.KStr {
			if v.k == kDyn && v.slot {
				return
			}
			*ref = e.strSlot(v)
			return
		}
		if v.k == kDyn && v.slot {
			return
		}
		id := e.newVar()
		e.emit(ir.Mov{Dst: id, A: opnd(v)})
		lo, hi := rng(v)
		*ref = &V{k: kDyn, ty: v.ty, v: id, lo: lo, hi: hi, slot: true}
	case kRope:
		*ref = e.strSlot(v)
	case kStruct, kSoA:
		for i := range v.elems {
			e.materialize(&v.elems[i])
		}
	case kArr:
		if v.ty.Elem.IsScalar() {
			e.toMem(v)
		} else {
			for i := range v.elems {
				e.materialize(&v.elems[i])
			}
		}
	}
}

// widen forgets range information of slot-backed storage (used at loop heads).
func (e *eng) widen(ref **V) {
	v := *ref
	switch v.k {
	case kDyn:
		if v.ty.Kind == types.KStr && v.slot {
			*ref = &V{k: kDyn, ty: v.ty, v: v.v, buf: v.buf, lo: big.NewInt(0), hi: big.NewInt(int64(v.ty.Cap)), slot: true, capDecl: v.capDecl}
		}
		if v.ty.IsScalar() && v.slot {
			lo, hi := v.ty.Range()
			*ref = &V{k: kDyn, ty: v.ty, v: v.v, lo: lo, hi: hi, slot: true}
		}
	case kStruct, kArr, kSoA:
		for i := range v.elems {
			e.widen(&v.elems[i])
		}
	case kMem:
		v.elo, v.ehi = v.ty.Elem.Range()
	}
}

// initInst builds the cheapest instruction that initialises array id with ops.
func initInst(id int, ops []ir.Operand) ir.Inst {
	if len(ops) > 4 {
		uniform := true
		for _, o := range ops[1:] {
			if o != ops[0] {
				uniform = false
				break
			}
		}
		if uniform {
			return ir.ArrFill{Arr: id, Val: ops[0]}
		}
	}
	return ir.ArrInit{Arr: id, Vals: ops}
}

// zeroV builds a zero value of type t out of constants (scalars) and tracked aggregates.
func (e *eng) zeroV(t *types.Type) *V {
	switch t.Kind {
	case types.KStruct:
		out := &V{k: kStruct, ty: t, elems: make([]*V, len(t.Fields))}
		for i, f := range t.Fields {
			out.elems[i] = e.zeroV(f.Ty)
		}
		return out
	case types.KArray:
		out := &V{k: kArr, ty: t, elems: make([]*V, t.N)}
		for i := range out.elems {
			out.elems[i] = e.zeroV(t.Elem)
		}
		return out
	case types.KFloat:
		return cFloat(t, 0)
	case types.KBool:
		return cBool(false)
	}
	if t.IsScalar() {
		return cInt(t, big.NewInt(0))
	}
	e.fail("E0502", "dynamic return of a value containing type `%s` is unsupported", t)
	return nil
}

// flatStructArr reports whether t is an array of structs whose fields are all scalars.
func flatStructArr(t *types.Type) bool {
	if t == nil || t.Kind != types.KArray || t.Elem.Kind != types.KStruct || len(t.Elem.Fields) == 0 {
		return false
	}
	for _, f := range t.Elem.Fields {
		if !f.Ty.IsScalar() {
			return false
		}
	}
	return true
}

// toSoA moves an array of flat structs into IR memory, one array per field, in place.
// It must run outside any captured region (see prepSoA) so the memory is always initialised.
func (e *eng) toSoA(v *V) {
	if v.k == kSoA {
		return
	}
	cols := make([]*V, len(v.ty.Elem.Fields))
	for f, fd := range v.ty.Elem.Fields {
		elems := make([]*V, len(v.elems))
		for k, el := range v.elems {
			elems[k] = el.elems[f]
		}
		col := &V{k: kArr, ty: types.Array(fd.Ty, v.ty.N), elems: elems}
		e.toMem(col)
		cols[f] = col
	}
	v.k, v.elems = kSoA, cols
}

// prepSoA converts every array of flat structs reachable from *ref to struct-of-arrays
// form. Regions call it for roots written through a run-time index, before the region
// is captured.
func (e *eng) prepSoA(v *V) {
	switch v.k {
	case kArr:
		if flatStructArr(v.ty) && v.ty.N >= 2 {
			e.toSoA(v)
			return
		}
		for _, el := range v.elems {
			e.prepSoA(el)
		}
	case kStruct:
		for _, el := range v.elems {
			e.prepSoA(el)
		}
	}
}

// soaElem reads element idx of an SoA array as a struct of scalar loads.
func (e *eng) soaElem(v, idx *V) *V {
	out := &V{k: kStruct, ty: v.ty.Elem, elems: make([]*V, len(v.elems))}
	for f, col := range v.elems {
		out.elems[f] = e.indexValue(col, idx)
	}
	return out
}

// toMem converts a scalar-element kArr into IR memory in place.
func (e *eng) toMem(v *V) {
	if v.k == kMem {
		return
	}
	if !v.ty.Elem.IsScalar() {
		e.fail("E0502", "dynamic indexing requires an array of scalars in v0.2")
	}
	id := e.newArr(v.ty.N, irTy(v.ty.Elem))
	ops := make([]ir.Operand, len(v.elems))
	var lo, hi *big.Int
	for i, el := range v.elems {
		ops[i] = opnd(el)
		l, h := rng(el)
		if lo == nil {
			lo, hi = l, h
		} else {
			lo, hi = bmin(lo, l), bmax(hi, h)
		}
	}
	e.emit(initInst(id, ops))
	v.k, v.mem, v.elems, v.elo, v.ehi = kMem, id, nil, lo, hi
}

// constTable interns an all-constant scalar array as a read-only table.
func (e *eng) constTable(v *V) (int, *big.Int, *big.Int) {
	ty := irTy(v.ty.Elem)
	vals := make([]uint64, len(v.elems))
	var lo, hi *big.Int
	for i, el := range v.elems {
		vals[i] = vbits(el)
		if lo == nil {
			lo, hi = el.c, el.c
		} else {
			lo, hi = bmin(lo, el.c), bmax(hi, el.c)
		}
	}
	key := ty.String() + constKey(v)
	if id, ok := e.tables[key]; ok {
		return id, lo, hi
	}
	e.tabList = append(e.tabList, ir.Table{Ty: ty, Vals: vals})
	e.tables[key] = len(e.tabList) - 1
	return len(e.tabList) - 1, lo, hi
}

// tableFor returns a memory array holding the contents of a kArr (read-only use).
func (e *eng) tableFor(v *V) (int, *big.Int, *big.Int) {
	if v.k == kMem {
		return v.mem, v.elo, v.ehi
	}
	if !v.ty.Elem.IsScalar() {
		e.fail("E0502", "dynamic indexing requires an array of scalars in v0.2")
	}
	var lo, hi *big.Int
	ops := make([]ir.Operand, len(v.elems))
	for i, el := range v.elems {
		ops[i] = opnd(el)
		l, h := rng(el)
		if lo == nil {
			lo, hi = l, h
		} else {
			lo, hi = bmin(lo, l), bmax(hi, h)
		}
	}
	id := e.newArr(v.ty.N, irTy(v.ty.Elem))
	e.emit(initInst(id, ops))
	return id, lo, hi
}
