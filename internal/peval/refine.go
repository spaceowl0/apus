package peval

import (
	"math/big"

	"apus/internal/syntax"
	"apus/internal/types"
)

// Path-sensitive range refinement: inside the true branch of `x < 10` the
// compiler knows x's upper bound is 9, which removes overflow, bounds and
// division checks that depend on it. All narrowing is undone (or deliberately
// kept, for guard clauses) by the caller.

var negateOp = map[string]string{"<": ">=", "<=": ">", ">": "<=", ">=": "<", "==": "!=", "!=": "=="}
var flipOp = map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<=", "==": "==", "!=": "!="}

// refine narrows variable ranges assuming cond evaluates to truth. The returned
// function restores the previous knowledge.
func (e *eng) refine(en *env, cond syntax.Expr, truth bool) func() {
	var undos []func()
	e.refineAtom(en, cond, truth, &undos)
	return func() {
		for i := len(undos) - 1; i >= 0; i-- {
			undos[i]()
		}
	}
}

func (e *eng) refineAtom(en *env, cond syntax.Expr, truth bool, undos *[]func()) {
	switch c := cond.(type) {
	case *syntax.Unary:
		if c.Op == "!" {
			e.refineAtom(en, c.X, !truth, undos)
		}
	case *syntax.Binary:
		switch c.Op {
		case "&&":
			if truth {
				e.refineAtom(en, c.L, true, undos)
				e.refineAtom(en, c.R, true, undos)
			}
		case "||":
			if !truth {
				e.refineAtom(en, c.L, false, undos)
				e.refineAtom(en, c.R, false, undos)
			}
		case "<", "<=", ">", ">=", "==", "!=":
			op := c.Op
			if !truth {
				op = negateOp[op]
			}
			lb, lv := e.refOperand(en, c.L)
			rb, rv := e.refOperand(en, c.R)
			if lv == nil || rv == nil {
				return
			}
			if lb != nil {
				e.narrow(lb, op, rv, undos)
			}
			if rb != nil {
				e.narrow(rb, flipOp[op], lv, undos)
			}
		}
	}
}

// refOperand resolves a comparison operand without evaluating anything: a local
// variable (narrowable), a literal or a global constant.
func (e *eng) refOperand(en *env, x syntax.Expr) (*binding, *V) {
	var v *V
	var b *binding
	switch n := x.(type) {
	case *syntax.IntLit:
		v = cInt(fixTy(n.Ty), n.V)
	case *syntax.Ident:
		if n.EnumVal != nil {
			v = cInt(n.Ty, n.EnumVal)
		} else if lb := en.lookup(n.Name); lb != nil {
			b, v = lb, lb.v
		} else if g, ok := e.gconst[n.Name]; ok {
			v = g
		}
	}
	if v == nil || (v.k != kConst && v.k != kDyn) || v.ty == nil || (v.ty.Kind != types.KInt && v.ty.Kind != types.KEnum) {
		return nil, nil
	}
	return b, v
}

func (e *eng) narrow(b *binding, op string, other *V, undos *[]func()) {
	old := b.v
	if old.k != kDyn {
		return
	}
	olo, ohi := rng(other)
	lo, hi := old.lo, old.hi
	one := big.NewInt(1)
	switch op {
	case "<":
		hi = bmin(hi, new(big.Int).Sub(ohi, one))
	case "<=":
		hi = bmin(hi, ohi)
	case ">":
		lo = bmax(lo, new(big.Int).Add(olo, one))
	case ">=":
		lo = bmax(lo, olo)
	case "==":
		lo, hi = bmax(lo, olo), bmin(hi, ohi)
	case "!=":
		if olo.Cmp(ohi) == 0 {
			if lo.Cmp(olo) == 0 {
				lo = new(big.Int).Add(lo, one)
			}
			if hi.Cmp(olo) == 0 {
				hi = new(big.Int).Sub(hi, one)
			}
		}
	}
	hole := op == "!=" && olo.Cmp(ohi) == 0 && olo.Cmp(lo) > 0 && olo.Cmp(hi) < 0 && old.excl == nil
	if lo.Cmp(hi) > 0 || (lo.Cmp(old.lo) == 0 && hi.Cmp(old.hi) == 0 && !hole) {
		return // unreachable path or no new information
	}
	nv := *old
	nv.lo, nv.hi = lo, hi
	if op == "!=" && olo.Cmp(ohi) == 0 && olo.Cmp(lo) > 0 && olo.Cmp(hi) < 0 && nv.excl == nil {
		nv.excl = olo // a hole strictly inside the interval
	}
	b.v = &nv
	*undos = append(*undos, func() { b.v = old })
}
