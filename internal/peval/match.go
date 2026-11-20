package peval

import (
	"fmt"
	"math/big"

	"apus/internal/syntax"
	"apus/internal/types"
)

// match is lowered to a chain of `if` statements over a (possibly hidden)
// scrutinee variable, so constant scrutinees fold to a single arm and runtime
// scrutinees reuse all dynamic-branch machinery (materialization, range joins,
// guard clauses, narrowing inside arms).

func (e *eng) patCond(scr syntax.Expr, st *types.Type, p *syntax.Pattern) syntax.Expr {
	lit := func(v *big.Int) syntax.Expr { return syntax.NewIntLit(p.Pos, v, st) }
	if p.Lo.Cmp(p.Hi) == 0 {
		return syntax.NewBinary(p.Pos, "==", scr, lit(p.Lo), types.Bool)
	}
	return syntax.NewBinary(p.Pos, "&&",
		syntax.NewBinary(p.Pos, ">=", scr, lit(p.Lo), types.Bool),
		syntax.NewBinary(p.Pos, "<=", scr, lit(p.Hi), types.Bool), types.Bool)
}

// armCond returns the condition selecting an arm, or nil for a wildcard arm.
func (e *eng) armCond(scr syntax.Expr, st *types.Type, a *syntax.Arm) syntax.Expr {
	var cond syntax.Expr
	for _, p := range a.Pats {
		if p.Wild {
			return nil
		}
		c := e.patCond(scr, st, p)
		if cond == nil {
			cond = c
		} else {
			cond = syntax.NewBinary(p.Pos, "||", cond, c, types.Bool)
		}
	}
	return cond
}

var hiddenN int

// scrutinee returns an expression naming the matched value, declaring a hidden
// variable when the scrutinee is not already a local variable.
func (e *eng) scrutinee(en *env, x syntax.Expr) syntax.Expr {
	if id, ok := x.(*syntax.Ident); ok && id.EnumVal == nil && en.lookup(id.Name) != nil {
		return id
	}
	v := e.own(e.expr(en, x))
	hiddenN++
	name := fmt.Sprintf("$m%d", hiddenN)
	en.declare(name, v)
	return syntax.NewIdent(x.Position(), name, x.Type())
}

func (e *eng) chain(scr syntax.Expr, st *types.Type, arms []*syntax.Arm, body func(*syntax.Arm) []syntax.Stmt) []syntax.Stmt {
	var build func(i int) []syntax.Stmt
	build = func(i int) []syntax.Stmt {
		a := arms[i]
		cond := e.armCond(scr, st, a)
		if cond == nil || i == len(arms)-1 {
			return body(a)
		}
		return []syntax.Stmt{&syntax.If{Pos: a.Pos, Cond: cond, Then: body(a), Else: build(i + 1)}}
	}
	return build(0)
}

func (e *eng) matchStmt(fr *frame, en *env, s *syntax.MatchStmt) ctl {
	en.push()
	scr := e.scrutinee(en, s.X)
	c := e.stmts(fr, en, e.chain(scr, s.X.Type(), s.Arms, func(a *syntax.Arm) []syntax.Stmt { return a.Body }))
	en.pop()
	return c
}

func (e *eng) matchExpr(en *env, n *syntax.MatchExpr) *V {
	st := n.X.Type()
	rt := fixTy(n.Ty)
	x := e.expr(en, n.X)
	if x.k == kConst { // statically known: evaluate only the selected arm
		e.stats.BranchesRemoved++
		for _, a := range n.Arms {
			for _, p := range a.Pats {
				if p.Wild || (x.c.Cmp(p.Lo) >= 0 && x.c.Cmp(p.Hi) <= 0) {
					return e.expr(en, a.Val)
				}
			}
		}
		e.fail("E0000", "internal: no match arm selected")
	}
	if !rt.IsScalar() {
		e.fail("E0502", "a `match` expression on a runtime value must produce a scalar in v0.3")
	}
	en.push()
	defer en.pop()
	hiddenN++
	name := fmt.Sprintf("$m%d", hiddenN)
	var scr syntax.Expr
	if id, ok := n.X.(*syntax.Ident); ok && id.EnumVal == nil && en.lookup(id.Name) != nil {
		scr = id
	} else {
		en.declare(name, e.own(x))
		scr = syntax.NewIdent(n.Pos, name, st)
	}
	hiddenN++
	res := fmt.Sprintf("$r%d", hiddenN)
	en.declare(res, cInt(rt, big.NewInt(0)))
	if rt.Kind == types.KBool {
		en.declare(res, cBool(false))
	}
	fr := &frame{retSlot: -1, flag: -1}
	stmts := e.chain(scr, st, n.Arms, func(a *syntax.Arm) []syntax.Stmt {
		return []syntax.Stmt{&syntax.Assign{Pos: a.Pos, Target: syntax.NewIdent(a.Pos, res, rt), X: a.Val}}
	})
	e.stmts(fr, en, stmts)
	return e.own(en.lookup(res).v)
}
