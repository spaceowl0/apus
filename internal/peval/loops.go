package peval

import (
	"math/big"

	"apus/internal/diag"
	"apus/internal/ir"
	"apus/internal/syntax"
)

var u8 = ir.IntTy{Bits: 8}

// stmts executes a statement list. After a dynamic return (inlined function) or
// a dynamic break/continue, the remaining statements run under a guard on the
// corresponding flags, with the same variable-merging rules as a dynamic `if`.
func (e *eng) stmts(fr *frame, en *env, list []syntax.Stmt) ctl {
	for i, st := range list {
		beforeR, beforeL := fr.dynRets, e.loopEvents(fr)
		c := e.stmt(fr, en, st)
		if c != cNone {
			return c
		}
		if i+1 < len(list) && ((fr.dynRets != beforeR && !fr.isMain) || e.loopEvents(fr) != beforeL) {
			rest := list[i+1:]
			names := map[string]bool{}
			assignedRoots(rest, names)
			return e.dynIf(fr, en, e.guardCond(fr), nil, names, func() ctl { return e.stmts(fr, en, rest) }, nil)
		}
	}
	return cNone
}

// loopEvents counts dynamic break/continue events of the innermost loop only.
func (e *eng) loopEvents(fr *frame) int {
	if lc := e.curLoop(fr); lc != nil {
		return lc.breaks + lc.continues
	}
	return 0
}

func (e *eng) notFlag(flag int) ir.Operand {
	t := e.newVar()
	e.emit(ir.Bin{Dst: t, Op: ir.Eq, A: ir.V(flag), B: ir.K(0), Ty: u8})
	return ir.V(t)
}

// guardCond is true while no return/break/continue flag relevant here is set.
func (e *eng) guardCond(fr *frame) ir.Operand {
	var terms []ir.Operand
	if !fr.isMain && fr.dynRets > 0 {
		terms = append(terms, e.notFlag(fr.flag))
	}
	if lc := e.curLoop(fr); lc != nil {
		if lc.hasBrk {
			terms = append(terms, e.notFlag(lc.brk))
		}
		if lc.hasCont {
			terms = append(terms, e.notFlag(lc.cont))
		}
	}
	return e.andAll(terms)
}

func (e *eng) andAll(terms []ir.Operand) ir.Operand {
	if len(terms) == 0 {
		return ir.K(1)
	}
	acc := terms[0]
	for _, t := range terms[1:] {
		d := e.newVar()
		e.emit(ir.Bin{Dst: d, Op: ir.And, A: acc, B: t, Ty: u8})
		acc = ir.V(d)
	}
	return acc
}

// loopGuard is true while neither this loop's break flag nor the function's
// "returned" flag is set; used to skip the remaining iterations of an unrolled loop.
func (e *eng) loopGuard(fr *frame, lc *loopCtx) ir.Operand {
	var terms []ir.Operand
	if lc.hasBrk {
		terms = append(terms, e.notFlag(lc.brk))
	}
	if !fr.isMain && fr.dynRets > 0 {
		terms = append(terms, e.notFlag(fr.flag))
	}
	return e.andAll(terms)
}

// dynIf emits a runtime conditional. names lists the variable roots that either
// branch may assign: they are materialized into slots first and their ranges
// joined afterwards. condAST (optional) enables path-sensitive range narrowing.
func (e *eng) dynIf(fr *frame, en *env, cond ir.Operand, condAST syntax.Expr, names map[string]bool, then, els func() ctl) ctl {
	var scalars []*binding
	var preLo, preHi []*big.Int
	for _, n := range sortedKeys(names) {
		b := en.lookup(n)
		if b == nil {
			continue
		}
		if names[n+"#"] {
			e.prepSoA(b.v)
		}
		e.materialize(&b.v)
		if b.v.k == kDyn && b.v.ty.IsScalar() {
			scalars = append(scalars, b)
			preLo = append(preLo, b.v.lo)
			preHi = append(preHi, b.v.hi)
		} else {
			e.widen(&b.v)
		}
	}
	var hullLo, hullHi []*big.Int
	runBranch := func(body func() ctl, truth bool) ([]ir.Inst, bool) {
		term := false
		undo := func() {}
		if condAST != nil {
			undo = e.refine(en, condAST, truth)
		}
		insts := e.capture(func() {
			if body == nil {
				return
			}
			switch body() {
			case cRet:
				e.dynReturn(fr, fr.retVal)
				fr.retVal = nil
				term = true
			case cDone:
				term = true
			case cBreak:
				e.dynBreak(fr)
			case cContinue:
				e.dynContinue(fr)
			}
		})
		if !term {
			for i, b := range scalars {
				if len(hullLo) <= i {
					hullLo = append(hullLo, b.v.lo)
					hullHi = append(hullHi, b.v.hi)
				} else {
					hullLo[i], hullHi[i] = bmin(hullLo[i], b.v.lo), bmax(hullHi[i], b.v.hi)
				}
			}
		}
		undo()
		for i, b := range scalars {
			b.v = &V{k: kDyn, ty: b.v.ty, v: b.v.v, lo: preLo[i], hi: preHi[i], slot: true}
		}
		return insts, term
	}
	thenI, thenT := runBranch(then, true)
	elseI, elseT := runBranch(els, false)
	e.emit(ir.If{Cond: cond, Then: thenI, Else: elseI})
	for i, b := range scalars {
		if i < len(hullLo) {
			b.v = &V{k: kDyn, ty: b.v.ty, v: b.v.v, lo: hullLo[i], hi: hullHi[i], slot: true}
		}
	}
	// guard clause: when exactly one branch exits, the continuing path knows the other condition
	if condAST != nil {
		if thenT && !elseT {
			e.refine(en, condAST, false)
		} else if elseT && !thenT {
			e.refine(en, condAST, true)
		}
	}
	if thenT && elseT {
		if fr.isMain {
			return cDone
		}
		return cRet
	}
	return cNone
}

func (e *eng) ifStmt(fr *frame, en *env, s *syntax.If) ctl {
	c := e.expr(en, s.Cond)
	e.pos = s.Pos
	if c.k == kConst {
		e.stats.BranchesRemoved++
		if c.c.Sign() != 0 {
			return e.scoped(fr, en, s.Then)
		}
		return e.scoped(fr, en, s.Else)
	}
	names := map[string]bool{}
	assignedRoots(s.Then, names)
	assignedRoots(s.Else, names)
	return e.dynIf(fr, en, opnd(c), s.Cond, names,
		func() ctl { return e.scoped(fr, en, s.Then) },
		func() ctl { return e.scoped(fr, en, s.Else) })
}

// ---------------------------------------------------------------- loop contexts

func scanLoopCtl(b []syntax.Stmt) (brk, cont bool) {
	for _, s := range b {
		switch s := s.(type) {
		case *syntax.Break:
			brk = true
		case *syntax.Continue:
			cont = true
		case *syntax.If:
			b1, c1 := scanLoopCtl(s.Then)
			b2, c2 := scanLoopCtl(s.Else)
			brk, cont = brk || b1 || b2, cont || c1 || c2
		case *syntax.Block:
			b1, c1 := scanLoopCtl(s.Body)
			brk, cont = brk || b1, cont || c1
		case *syntax.Region:
			b1, c1 := scanLoopCtl(s.Body)
			brk, cont = brk || b1, cont || c1
		case *syntax.MatchStmt:
			for _, a := range s.Arms {
				b1, c1 := scanLoopCtl(a.Body)
				brk, cont = brk || b1, cont || c1
			}
		}
	}
	return
}

func (e *eng) pushLoop(fr *frame, body []syntax.Stmt) *loopCtx {
	hb, hc := scanLoopCtl(body)
	lc := &loopCtx{brk: -1, cont: -1, hasBrk: hb, hasCont: hc}
	if hb {
		lc.brk = e.newVar()
		e.emit(ir.Mov{Dst: lc.brk, A: ir.K(0)})
	}
	if hc {
		lc.cont = e.newVar()
	}
	fr.loops = append(fr.loops, lc)
	return lc
}

func (e *eng) popLoop(fr *frame) { fr.loops = fr.loops[:len(fr.loops)-1] }

func (e *eng) curLoop(fr *frame) *loopCtx {
	if len(fr.loops) == 0 {
		return nil
	}
	return fr.loops[len(fr.loops)-1]
}

func (e *eng) dynBreak(fr *frame) {
	lc := e.curLoop(fr)
	e.emit(ir.Mov{Dst: lc.brk, A: ir.K(1)})
	lc.breaks++
}

func (e *eng) dynContinue(fr *frame) {
	lc := e.curLoop(fr)
	e.emit(ir.Mov{Dst: lc.cont, A: ir.K(1)})
	lc.continues++
}

// loopCond folds the break flag into a residual loop's condition so that no
// condition code (which may read input) runs after a break.
func (e *eng) loopCond(fr *frame, lc *loopCtx, retInBody bool, pre []ir.Inst, c ir.Operand) ([]ir.Inst, ir.Operand) {
	var flags []int
	if lc.hasBrk {
		flags = append(flags, lc.brk)
	}
	if retInBody && !fr.isMain {
		flags = append(flags, fr.flag) // a `return` inside the body also ends the loop
	}
	if len(flags) == 0 {
		return pre, c
	}
	cs := e.newVar()
	insts := []ir.Inst{ir.Mov{Dst: cs, A: ir.K(0)}}
	var acc ir.Operand
	for i, f := range flags {
		nb := e.newVar()
		insts = append(insts, ir.Bin{Dst: nb, Op: ir.Eq, A: ir.V(f), B: ir.K(0), Ty: u8})
		if i == 0 {
			acc = ir.V(nb)
			continue
		}
		both := e.newVar()
		insts = append(insts, ir.Bin{Dst: both, Op: ir.And, A: acc, B: ir.V(nb), Ty: u8})
		acc = ir.V(both)
	}
	body := append(append([]ir.Inst{}, pre...), ir.Mov{Dst: cs, A: c})
	return append(insts, ir.If{Cond: acc, Then: body}), ir.V(cs)
}

// ---------------------------------------------------------------- while

func (e *eng) whileStmt(fr *frame, en *env, s *syntax.While) ctl {
	lc := e.pushLoop(fr, s.Body)
	defer e.popLoop(fr)
	names := map[string]bool{}
	assignedRoots(s.Body, names)
	var run func(iter int) ctl
	run = func(iter int) ctl {
		for {
			var c *V
			pre := e.capture(func() { c = e.expr(en, s.Cond) })
			e.pos = s.Pos
			if c.k != kConst {
				return e.dynLoop(fr, en, lc, s.Cond, s.Body, s.Bound-iter, s.Pos)
			}
			e.buf = append(e.buf, pre...)
			if c.c.Sign() == 0 {
				return cNone
			}
			if iter >= s.Bound {
				e.failh("E0201", "raise `bound(N)` or fix the loop condition",
					"loop exceeds its declared bound(%d)", s.Bound)
			}
			if iter >= e.opt.UnrollLimit && e.static == 0 {
				return e.dynLoop(fr, en, lc, s.Cond, s.Body, s.Bound-iter, s.Pos)
			}
			if lc.hasCont {
				e.emit(ir.Mov{Dst: lc.cont, A: ir.K(0)})
			}
			before, rbefore := lc.breaks, fr.dynRets
			fr.loopDepth++
			cc := e.scoped(fr, en, s.Body)
			fr.loopDepth--
			e.stats.UnrolledIters++
			iter++
			switch cc {
			case cBreak:
				return cNone
			case cRet, cDone:
				return cc
			}
			if lc.breaks != before || (!fr.isMain && fr.dynRets != rbefore) {
				next := iter
				return e.dynIf(fr, en, e.loopGuard(fr, lc), nil, names, func() ctl { return run(next) }, nil)
			}
		}
	}
	return run(0)
}

// dynLoop emits a residual loop. Variables assigned in the body are
// materialized and widened (sound for any iteration); the body is evaluated once.
func (e *eng) dynLoop(fr *frame, en *env, lc *loopCtx, cond syntax.Expr, body []syntax.Stmt, bound int, pos diag.Pos) ctl {
	names := e.prepareLoop(en, body)
	before := e.snapshotArenas()
	var c *V
	pre := e.capture(func() { c = e.expr(en, cond) })
	e.pos = pos
	if c.k == kConst && c.c.Sign() == 0 {
		e.buf = append(e.buf, pre...)
		return cNone
	}
	fr.loopDepth++
	rbefore := fr.dynRets
	bodyI := e.capture(func() {
		if lc.hasCont {
			e.emit(ir.Mov{Dst: lc.cont, A: ir.K(0)})
		}
		undo := e.refine(en, cond, true)
		cc := e.scoped(fr, en, body)
		undo()
		e.loopExit(fr, cc)
	})
	fr.loopDepth--
	e.loopArenas(before, big.NewInt(int64(bound)))
	pre, cop := e.loopCond(fr, lc, fr.dynRets != rbefore, pre, opnd(c))
	e.stats.DynLoops++
	e.emit(ir.While{Pre: pre, Cond: cop, Body: bodyI, Bound: bound})
	e.finishLoop(en, names)
	return cNone
}

// loopExit handles control flow that ends the evaluated loop body statically.
func (e *eng) loopExit(fr *frame, cc ctl) {
	switch cc {
	case cRet:
		e.dynReturn(fr, fr.retVal)
	case cBreak:
		lc := e.curLoop(fr)
		e.emit(ir.Mov{Dst: lc.brk, A: ir.K(1)})
	}
}

// ---------------------------------------------------------------- for

func (e *eng) forStmt(fr *frame, en *env, s *syntax.For) ctl {
	lo := e.expr(en, s.Lo)
	hi := e.expr(en, s.Hi)
	e.pos = s.Pos
	ty := s.Ty
	names := map[string]bool{}
	assignedRoots(s.Body, names)
	one := big.NewInt(1)
	if lo.k == kConst && hi.k == kConst {
		n := new(big.Int).Sub(hi.c, lo.c)
		if n.Sign() <= 0 {
			return cNone
		}
		if n.Cmp(big.NewInt(int64(e.opt.UnrollLimit))) <= 0 || e.static > 0 {
			lc := e.pushLoop(fr, s.Body)
			defer e.popLoop(fr)
			var run func(i *big.Int) ctl
			run = func(i *big.Int) ctl {
				for ; i.Cmp(hi.c) < 0; i = new(big.Int).Add(i, one) {
					if lc.hasCont {
						e.emit(ir.Mov{Dst: lc.cont, A: ir.K(0)})
					}
					before, rbefore := lc.breaks, fr.dynRets
					fr.loopDepth++
					en.push()
					en.declare(s.Var, cInt(ty, new(big.Int).Set(i)))
					c := e.stmts(fr, en, s.Body)
					en.pop()
					fr.loopDepth--
					e.stats.UnrolledIters++
					switch c {
					case cBreak:
						return cNone
					case cRet, cDone:
						return c
					}
					if lc.breaks != before || (!fr.isMain && fr.dynRets != rbefore) {
						next := new(big.Int).Add(i, one)
						if next.Cmp(hi.c) < 0 {
							return e.dynIf(fr, en, e.loopGuard(fr, lc), nil, names, func() ctl { return run(next) }, nil)
						}
					}
				}
				return cNone
			}
			return run(new(big.Int).Set(lo.c))
		}
	}
	llo, _ := rng(lo)
	_, hhi := rng(hi)
	_, thi := ty.Range()
	if hi.k == kDyn && hhi.Cmp(thi) == 0 {
		e.failh("E0201", "bound the upper limit with `clamp(n, lo, hi)` or `require(n, lo, hi)`",
			"loop trip count has no provable upper bound")
	}
	trips := new(big.Int).Sub(hhi, llo)
	if trips.Sign() <= 0 {
		return cNone
	}
	lc := e.pushLoop(fr, s.Body)
	defer e.popLoop(fr)
	islot := e.newVar()
	e.emit(ir.Mov{Dst: islot, A: opnd(lo)})
	hop := opnd(hi)
	if hi.k != kConst {
		h := e.newVar()
		e.emit(ir.Mov{Dst: h, A: opnd(hi)})
		hop = ir.V(h)
	}
	pnames := e.prepareLoop(en, s.Body)
	before := e.snapshotArenas()
	cond := e.newVar()
	pre := []ir.Inst{ir.Bin{Dst: cond, Op: ir.Lt, A: ir.V(islot), B: hop, Ty: irTy(ty)}}
	fr.loopDepth++
	rbefore := fr.dynRets
	ivar := &V{k: kDyn, ty: ty, v: islot, lo: llo, hi: new(big.Int).Sub(hhi, one), slot: true}
	bodyI := e.capture(func() {
		if lc.hasCont {
			e.emit(ir.Mov{Dst: lc.cont, A: ir.K(0)})
		}
		en.push()
		en.declare(s.Var, ivar)
		cc := e.stmts(fr, en, s.Body)
		en.pop()
		e.loopExit(fr, cc)
		e.emit(ir.Bin{Dst: islot, Op: ir.Add, A: ir.V(islot), B: ir.K(1), Ty: irTy(ty)})
	})
	fr.loopDepth--
	e.loopArenas(before, trips)
	pre, cop := e.loopCond(fr, lc, fr.dynRets != rbefore, pre, ir.V(cond))
	e.stats.DynLoops++
	e.emit(ir.While{Pre: pre, Cond: cop, Body: bodyI, Bound: -1})
	e.finishLoop(en, pnames)
	return cNone
}
