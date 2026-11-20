package syntax

// Deep cloning of function declarations, used to instantiate generic functions:
// every instance gets its own AST so type annotations never collide.

func cloneTE(t *TypeExpr) *TypeExpr {
	if t == nil {
		return nil
	}
	c := *t
	c.N = CloneExpr(t.N)
	c.Elem = cloneTE(t.Elem)
	return &c
}

func cloneExprs(xs []Expr) []Expr {
	out := make([]Expr, len(xs))
	for i, x := range xs {
		out[i] = CloneExpr(x)
	}
	return out
}

func cloneArms(arms []*Arm) []*Arm {
	out := make([]*Arm, len(arms))
	for i, a := range arms {
		na := &Arm{Pos: a.Pos, Body: CloneStmts(a.Body), Val: CloneExpr(a.Val)}
		for _, p := range a.Pats {
			na.Pats = append(na.Pats, &Pattern{Pos: p.Pos, Wild: p.Wild, LoX: CloneExpr(p.LoX), HiX: CloneExpr(p.HiX)})
		}
		out[i] = na
	}
	return out
}

// CloneExpr returns an unannotated deep copy of e.
func CloneExpr(e Expr) Expr {
	if e == nil {
		return nil
	}
	switch n := e.(type) {
	case *IntLit:
		return &IntLit{base: base{Pos: n.Pos}, V: n.V}
	case *FloatLit:
		return &FloatLit{base: base{Pos: n.Pos}, Text: n.Text}
	case *StrLit:
		return &StrLit{base: base{Pos: n.Pos}, V: n.V}
	case *BoolLit:
		return &BoolLit{base: base{Pos: n.Pos}, V: n.V}
	case *Ident:
		return &Ident{base: base{Pos: n.Pos}, Name: n.Name}
	case *Unary:
		return &Unary{base: base{Pos: n.Pos}, Op: n.Op, X: CloneExpr(n.X)}
	case *Binary:
		return &Binary{base: base{Pos: n.Pos}, Op: n.Op, L: CloneExpr(n.L), R: CloneExpr(n.R)}
	case *Call:
		c := &Call{base: base{Pos: n.Pos}, Name: n.Name, Args: cloneExprs(n.Args)}
		for _, ta := range n.TypeArgs {
			c.TypeArgs = append(c.TypeArgs, GenericArg{TE: cloneTE(ta.TE), Val: CloneExpr(ta.Val)})
		}
		return c
	case *Cast:
		return &Cast{base: base{Pos: n.Pos}, X: CloneExpr(n.X), To: cloneTE(n.To)}
	case *Index:
		return &Index{base: base{Pos: n.Pos}, X: CloneExpr(n.X), I: CloneExpr(n.I)}
	case *Field:
		return &Field{base: base{Pos: n.Pos}, X: CloneExpr(n.X), Name: n.Name, Index: -1}
	case *StructLit:
		c := &StructLit{base: base{Pos: n.Pos}, Name: n.Name}
		for _, f := range n.Fields {
			c.Fields = append(c.Fields, FieldInit{Pos: f.Pos, Name: f.Name, X: CloneExpr(f.X)})
		}
		return c
	case *ArrayLit:
		return &ArrayLit{base: base{Pos: n.Pos}, Elems: cloneExprs(n.Elems)}
	case *ArrayRep:
		return &ArrayRep{base: base{Pos: n.Pos}, Val: CloneExpr(n.Val), N: CloneExpr(n.N)}
	case *Static:
		return &Static{base: base{Pos: n.Pos}, X: CloneExpr(n.X)}
	case *MatchExpr:
		return &MatchExpr{base: base{Pos: n.Pos}, X: CloneExpr(n.X), Arms: cloneArms(n.Arms)}
	}
	panic("syntax: CloneExpr: unhandled expression")
}

// CloneStmts returns an unannotated deep copy of a statement list.
func CloneStmts(ss []Stmt) []Stmt {
	if ss == nil {
		return nil
	}
	out := make([]Stmt, len(ss))
	for i, s := range ss {
		out[i] = cloneStmt(s)
	}
	return out
}

func cloneStmt(s Stmt) Stmt {
	switch n := s.(type) {
	case *Let:
		return &Let{Pos: n.Pos, Name: n.Name, Mut: n.Mut, TE: cloneTE(n.TE), Init: CloneExpr(n.Init)}
	case *Assign:
		return &Assign{Pos: n.Pos, Target: CloneExpr(n.Target), X: CloneExpr(n.X)}
	case *ExprStmt:
		return &ExprStmt{Pos: n.Pos, X: CloneExpr(n.X)}
	case *If:
		return &If{Pos: n.Pos, Cond: CloneExpr(n.Cond), Then: CloneStmts(n.Then), Else: CloneStmts(n.Else)}
	case *While:
		return &While{Pos: n.Pos, Cond: CloneExpr(n.Cond), Bound: n.Bound, Body: CloneStmts(n.Body)}
	case *For:
		return &For{Pos: n.Pos, Var: n.Var, Lo: CloneExpr(n.Lo), Hi: CloneExpr(n.Hi), Body: CloneStmts(n.Body)}
	case *Return:
		return &Return{Pos: n.Pos, X: CloneExpr(n.X)}
	case *Region:
		return &Region{Pos: n.Pos, Arena: n.Arena, Body: CloneStmts(n.Body)}
	case *Block:
		return &Block{Pos: n.Pos, Body: CloneStmts(n.Body)}
	case *Break:
		return &Break{Pos: n.Pos}
	case *Continue:
		return &Continue{Pos: n.Pos}
	case *MatchStmt:
		return &MatchStmt{Pos: n.Pos, X: CloneExpr(n.X), Arms: cloneArms(n.Arms)}
	}
	panic("syntax: cloneStmt: unhandled statement")
}

// CloneFn returns an independent copy of f with all annotations cleared.
func CloneFn(f *FnDecl) *FnDecl {
	c := *f
	c.Params = make([]Param, len(f.Params))
	for i, p := range f.Params {
		c.Params[i] = Param{Name: p.Name, TE: cloneTE(p.TE), Lo: p.Lo, Hi: p.Hi}
	}
	c.RetTE = cloneTE(f.RetTE)
	c.Ret = nil
	c.Body = CloneStmts(f.Body)
	c.Generics = nil
	return &c
}
