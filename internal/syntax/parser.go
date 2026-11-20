package syntax

import (
	"fmt"
	"math/big"

	"apus/internal/diag"
)

var binPrec = map[string]int{
	"||": 1, "&&": 2,
	"==": 3, "!=": 3, "<": 4, "<=": 4, ">": 4, ">=": 4,
	"|": 5, "^": 6, "&": 7, "<<": 8, ">>": 8,
	"++": 9, "+": 10, "-": 10, "*": 11, "/": 11, "%": 11,
}

type Parser struct {
	iterN    int // counter for the hidden names of `for x in array` desugaring
	t        []Token
	i        int
	noStruct bool
}

// Parse parses a whole program. It panics with *diag.Error on failure.
func Parse(src string) *Program {
	p := &Parser{t: Lex(src)}
	return p.program()
}

func (p *Parser) peek() Token { return p.t[p.i] }
func (p *Parser) peekN(n int) Token {
	if p.i+n >= len(p.t) {
		return p.t[len(p.t)-1]
	}
	return p.t[p.i+n]
}
func (p *Parser) next() Token {
	t := p.t[p.i]
	if p.i < len(p.t)-1 {
		p.i++
	}
	return t
}
func (p *Parser) at(s string) bool {
	t := p.peek()
	return (t.Kind == Op || t.Kind == Ident_) && t.Text == s
}
func (p *Parser) accept(s string) bool {
	if p.at(s) {
		p.next()
		return true
	}
	return false
}
func (p *Parser) fail(t Token, f string, a ...any) {
	diag.Fail(diag.Errf("E0002", t.Pos, f, a...))
}
func (p *Parser) expect(s string) Token {
	t := p.next()
	if !((t.Kind == Op || t.Kind == Ident_) && t.Text == s) {
		desc := t.Text
		if t.Kind == EOF {
			desc = "end of file"
		}
		p.fail(t, "expected `%s`, found `%s`", s, desc)
	}
	return t
}
func (p *Parser) ident() Token {
	t := p.next()
	if t.Kind != Ident_ {
		p.fail(t, "expected identifier, found `%s`", t.Text)
	}
	return t
}
func (p *Parser) intLit() *big.Int {
	t := p.next()
	if t.Kind != Int_ {
		p.fail(t, "expected integer literal, found `%s`", t.Text)
	}
	return t.Val
}

// signedLit parses an optionally negative integer literal.
func (p *Parser) signedLit() *big.Int {
	if p.accept("-") {
		return new(big.Int).Neg(p.intLit())
	}
	return p.intLit()
}

func (p *Parser) program() *Program {
	prog := &Program{}
	for p.peek().Kind != EOF {
		t := p.peek()
		switch {
		case p.accept("const"):
			name := p.ident()
			p.expect(":")
			te := p.typeExpr()
			p.expect("=")
			x := p.expr()
			p.expect(";")
			prog.Consts = append(prog.Consts, &ConstDecl{Pos: t.Pos, Name: name.Text, TE: te, X: x})
		case p.accept("enum"):
			name := p.ident()
			p.expect("{")
			ed := &EnumDecl{Pos: t.Pos, Name: name.Text}
			for !p.at("}") {
				v := VariantDecl{Name: p.ident().Text}
				if p.accept("=") {
					v.Val = p.intLit()
				}
				ed.Variants = append(ed.Variants, v)
				if !p.accept(",") {
					break
				}
			}
			p.expect("}")
			if len(ed.Variants) == 0 {
				p.fail(t, "enum `%s` has no variants", name.Text)
			}
			prog.Enums = append(prog.Enums, ed)
		case p.accept("struct"):
			name := p.ident()
			p.expect("{")
			sd := &StructDecl{Pos: t.Pos, Name: name.Text}
			for !p.at("}") {
				fn := p.ident()
				p.expect(":")
				sd.Fields = append(sd.Fields, FieldDecl{fn.Text, p.typeExpr()})
				if !p.accept(",") {
					break
				}
			}
			p.expect("}")
			prog.Structs = append(prog.Structs, sd)
		case p.accept("arena"):
			name := p.ident()
			p.expect("(")
			sz := p.intLit()
			p.expect(")")
			p.expect(";")
			if !sz.IsInt64() || sz.Sign() <= 0 || sz.Int64() > 1<<31 {
				p.fail(t, "invalid arena size")
			}
			prog.Arenas = append(prog.Arenas, &ArenaDecl{Pos: t.Pos, Name: name.Text, Size: int(sz.Int64())})
		case p.at("fn") || p.at("pure"):
			prog.Fns = append(prog.Fns, p.fn())
		default:
			p.fail(t, "expected item (`fn`, `pure fn`, `const`, `struct`, `arena`), found `%s`", t.Text)
		}
	}
	return prog
}

// genericArgs parses `<A, 4, [u8; 2]>` after `::`.
func (p *Parser) genericArgs() []GenericArg {
	p.expect("<")
	var out []GenericArg
	for !p.at(">") {
		if t := p.peek(); t.Kind == Int_ || t.Text == "-" {
			out = append(out, GenericArg{Val: p.binary(10)})
		} else {
			out = append(out, GenericArg{TE: p.typeExpr()})
		}
		if !p.accept(",") {
			break
		}
	}
	p.expect(">")
	return out
}

func (p *Parser) typeExpr() *TypeExpr {
	t := p.peek()
	if p.accept("[") {
		el := p.typeExpr()
		p.expect(";")
		n := p.expr()
		p.expect("]")
		return &TypeExpr{Pos: t.Pos, Kind: TArray, Elem: el, N: n}
	}
	name := p.ident()
	if name.Text == "Str" && p.accept("<") {
		n := p.binary(5) // above comparison precedence so `>` closes the type
		p.expect(">")
		return &TypeExpr{Pos: t.Pos, Kind: TStr, N: n}
	}
	return &TypeExpr{Pos: t.Pos, Kind: TNamed, Name: name.Text}
}

func (p *Parser) fn() *FnDecl {
	t := p.peek()
	f := &FnDecl{Pos: t.Pos, BoundRec: -1}
	f.Pure = p.accept("pure")
	p.expect("fn")
	f.Name = p.ident().Text
	if p.accept("<") {
		for !p.at(">") {
			g := Generic{Pos: p.peek().Pos}
			if p.accept("const") {
				g.Const = true
				g.Name = p.ident().Text
				p.expect(":")
				g.ConstTE = p.typeExpr()
			} else {
				g.Name = p.ident().Text
			}
			f.Generics = append(f.Generics, g)
			if !p.accept(",") {
				break
			}
		}
		p.expect(">")
		if len(f.Generics) == 0 {
			p.fail(p.peek(), "empty generic parameter list")
		}
	}
	p.expect("(")
	for !p.at(")") {
		pn := p.ident()
		p.expect(":")
		prm := Param{Name: pn.Text, TE: p.typeExpr()}
		if p.accept("in") {
			prm.Lo = p.signedLit()
			p.expect("..=")
			prm.Hi = p.signedLit()
		}
		f.Params = append(f.Params, prm)
		if !p.accept(",") {
			break
		}
	}
	p.expect(")")
	if p.accept("->") {
		f.RetTE = p.typeExpr()
	}
	for {
		if p.accept("io") {
			f.IO = true
		} else if p.accept("bound") {
			p.expect("(")
			if p.at("rec") {
				p.next()
				p.expect("=")
			}
			n := p.intLit()
			p.expect(")")
			f.BoundRec = int(n.Int64())
		} else {
			break
		}
	}
	f.Body = p.block()
	f.NestedRet = false
	for _, s := range f.Body {
		if hasReturn(s, true) {
			f.NestedRet = true
		}
	}
	return f
}

// hasReturn reports whether s contains a return statement; top says s is a
// top-level statement of the function body (where a bare return is not "nested").
func hasReturn(s Stmt, top bool) bool {
	switch s := s.(type) {
	case *Return:
		return !top
	case *If:
		return anyRet(s.Then) || anyRet(s.Else)
	case *While:
		return anyRet(s.Body)
	case *For:
		return anyRet(s.Body)
	case *Region:
		return anyRet(s.Body)
	case *Block:
		return anyRet(s.Body)
	case *MatchStmt:
		for _, a := range s.Arms {
			if anyRet(a.Body) {
				return true
			}
		}
	}
	return false
}

func anyRet(b []Stmt) bool {
	for _, s := range b {
		if _, ok := s.(*Return); ok || hasReturn(s, false) {
			return true
		}
	}
	return false
}

func (p *Parser) block() []Stmt {
	p.expect("{")
	save := p.noStruct
	p.noStruct = false
	var out []Stmt
	for !p.at("}") {
		if p.peek().Kind == EOF {
			p.fail(p.peek(), "unexpected end of file in block")
		}
		out = append(out, p.stmt())
	}
	p.expect("}")
	p.noStruct = save
	return out
}

func (p *Parser) condExpr() Expr {
	save := p.noStruct
	p.noStruct = true
	e := p.expr()
	p.noStruct = save
	return e
}

func (p *Parser) stmt() Stmt {
	t := p.peek()
	switch {
	case p.accept("let"):
		s := &Let{Pos: t.Pos}
		s.Mut = p.accept("mut")
		s.Name = p.ident().Text
		if p.accept(":") {
			s.TE = p.typeExpr()
		}
		p.expect("=")
		s.Init = p.expr()
		p.expect(";")
		return s
	case p.accept("return"):
		s := &Return{Pos: t.Pos}
		if !p.at(";") {
			s.X = p.expr()
		}
		p.expect(";")
		return s
	case p.at("if"):
		return p.ifStmt()
	case p.at("{"):
		return &Block{Pos: t.Pos, Body: p.block()}
	case p.accept("break"):
		p.expect(";")
		return &Break{Pos: t.Pos}
	case p.accept("continue"):
		p.expect(";")
		return &Continue{Pos: t.Pos}
	case p.at("match"):
		p.next()
		x, arms := p.matchBody(true)
		return &MatchStmt{Pos: t.Pos, X: x, Arms: arms}
	case p.accept("while"):
		s := &While{Pos: t.Pos, Bound: -1}
		s.Cond = p.condExpr()
		p.expect("bound")
		p.expect("(")
		s.Bound = int(p.intLit().Int64())
		p.expect(")")
		s.Body = p.block()
		return s
	case p.accept("for"):
		s := &For{Pos: t.Pos}
		s.Var = p.ident().Text
		p.expect("in")
		s.Lo = p.condRange()
		if !p.at("..") {
			// for x in <array> { body }: iterate over a snapshot of the array
			p.iterN++
			arr, idx := fmt.Sprintf("$it%d", p.iterN), fmt.Sprintf("$i%d", p.iterN)
			body := p.block()
			ident := func(n string) *Ident { return &Ident{base: base{Pos: t.Pos}, Name: n} }
			elem := &Let{Pos: t.Pos, Name: s.Var, Init: &Index{base: base{Pos: t.Pos}, X: ident(arr), I: ident(idx)}}
			loop := &For{Pos: t.Pos, Var: idx,
				Lo:   &IntLit{base: base{Pos: t.Pos}, V: big.NewInt(0)},
				Hi:   &Call{base: base{Pos: t.Pos}, Name: "len", Args: []Expr{ident(arr)}},
				Body: append([]Stmt{elem}, body...)}
			return &Block{Pos: t.Pos, Body: []Stmt{&Let{Pos: t.Pos, Name: arr, Init: s.Lo}, loop}}
		}
		p.expect("..")
		s.Hi = p.condExpr()
		s.Body = p.block()
		return s
	case p.accept("region"):
		s := &Region{Pos: t.Pos}
		s.Arena = p.ident().Text
		s.Body = p.block()
		return s
	}
	x := p.expr()
	return p.finishSimple(t, x, true)
}

// finishSimple completes an expression statement or assignment whose left side
// (or whole expression) x has been parsed. semi says whether a `;` is required.
func (p *Parser) finishSimple(t Token, x Expr, semi bool) Stmt {
	for _, op := range []string{"=", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^="} {
		if p.at(op) {
			pt := p.next()
			rhs := p.expr()
			if semi {
				p.expect(";")
			}
			switch x.(type) {
			case *Ident, *Index, *Field:
			default:
				p.fail(pt, "invalid assignment target")
			}
			if op != "=" {
				rhs = &Binary{base: base{Pos: pt.Pos}, Op: op[:len(op)-1], L: x, R: rhs}
			}
			return &Assign{Pos: t.Pos, Target: x, X: rhs}
		}
	}
	if semi {
		p.expect(";")
	}
	return &ExprStmt{Pos: t.Pos, X: x}
}

// condRange parses the lower bound of a range; `..` is not a binary operator so
// plain expr() stops before it.
func (p *Parser) condRange() Expr { return p.condExpr() }

func (p *Parser) ifStmt() Stmt {
	t := p.expect("if")
	s := &If{Pos: t.Pos}
	s.Cond = p.condExpr()
	s.Then = p.block()
	if p.accept("else") {
		if p.at("if") {
			s.Else = []Stmt{p.ifStmt()}
		} else {
			s.Else = p.block()
		}
	}
	return s
}

func (p *Parser) expr() Expr { return p.binary(1) }

// matchBody parses `scrutinee { arms }`. In statement form arm bodies are
// blocks or single statements; in expression form they are expressions.
func (p *Parser) matchBody(stmtForm bool) (Expr, []*Arm) {
	x := p.condExpr()
	p.expect("{")
	save := p.noStruct
	p.noStruct = false
	var arms []*Arm
	for !p.at("}") {
		arm := &Arm{Pos: p.peek().Pos}
		for {
			pt := &Pattern{Pos: p.peek().Pos}
			if p.at("_") {
				p.next()
				pt.Wild = true
			} else {
				pt.LoX = p.unary()
				if p.accept("..=") {
					pt.HiX = p.unary()
				}
			}
			arm.Pats = append(arm.Pats, pt)
			if !p.accept("|") {
				break
			}
		}
		p.expect("=>")
		viaBlock := false
		if stmtForm {
			bt := p.peek()
			switch {
			case p.at("{"):
				viaBlock = true
				arm.Body = p.block()
			case p.accept("break"):
				arm.Body = []Stmt{&Break{Pos: bt.Pos}}
			case p.accept("continue"):
				arm.Body = []Stmt{&Continue{Pos: bt.Pos}}
			case p.accept("return"):
				r := &Return{Pos: bt.Pos}
				if !p.at(",") && !p.at("}") {
					r.X = p.expr()
				}
				arm.Body = []Stmt{r}
			default:
				arm.Body = []Stmt{p.finishSimple(bt, p.expr(), false)}
			}
		} else {
			arm.Val = p.expr()
		}
		arms = append(arms, arm)
		if !p.accept(",") && !p.at("}") && !viaBlock {
			p.fail(p.peek(), "expected `,` or `}` after match arm, found `%s`", p.peek().Text)
		}
	}
	p.expect("}")
	p.noStruct = save
	if len(arms) == 0 {
		p.fail(p.peek(), "match needs at least one arm")
	}
	return x, arms
}

func (p *Parser) binary(minPrec int) Expr {
	left := p.cast()
	for {
		t := p.peek()
		prec, ok := binPrec[t.Text]
		if t.Kind != Op || !ok || prec < minPrec {
			return left
		}
		p.next()
		right := p.binary(prec + 1)
		left = &Binary{base: base{Pos: t.Pos}, Op: t.Text, L: left, R: right}
	}
}

func (p *Parser) cast() Expr {
	x := p.unary()
	for p.at("as") {
		t := p.next()
		x = &Cast{base: base{Pos: t.Pos}, X: x, To: p.typeExpr()}
	}
	return x
}

func (p *Parser) unary() Expr {
	t := p.peek()
	switch {
	case p.accept("-"):
		x := p.unary()
		if il, ok := x.(*IntLit); ok {
			return &IntLit{base: base{Pos: t.Pos}, V: new(big.Int).Neg(il.V)}
		}
		return &Unary{base: base{Pos: t.Pos}, Op: "-", X: x}
	case p.accept("!"):
		return &Unary{base: base{Pos: t.Pos}, Op: "!", X: p.unary()}
	case p.accept("static"):
		return &Static{base: base{Pos: t.Pos}, X: p.unary()}
	}
	return p.postfix()
}

func (p *Parser) postfix() Expr {
	x := p.primary()
	for {
		t := p.peek()
		switch {
		case p.at("["):
			p.next()
			save := p.noStruct
			p.noStruct = false
			i := p.expr()
			p.noStruct = save
			p.expect("]")
			x = &Index{base: base{Pos: t.Pos}, X: x, I: i}
		case p.at(".") && p.peekN(1).Kind == Ident_:
			p.next()
			x = &Field{base: base{Pos: t.Pos}, X: x, Name: p.ident().Text, Index: -1}
		default:
			return x
		}
	}
}

func (p *Parser) primary() Expr {
	t := p.next()
	b := base{Pos: t.Pos}
	switch t.Kind {
	case Int_:
		return &IntLit{base: b, V: t.Val}
	case Float_:
		return &FloatLit{base: b, Text: t.Text}
	case Str_:
		return &StrLit{base: b, V: t.Text}
	case Ident_:
		if t.Text == "true" || t.Text == "false" {
			return &BoolLit{base: b, V: t.Text == "true"}
		}
		if t.Text == "match" {
			x, arms := p.matchBody(false)
			return &MatchExpr{base: b, X: x, Arms: arms}
		}
		name := t.Text
		var targs []GenericArg
		for p.at("::") {
			p.next()
			if p.at("<") {
				targs = p.genericArgs()
				break
			}
			name += "::" + p.ident().Text
		}
		if len(targs) > 0 && !p.at("(") {
			p.fail(p.peek(), "generic arguments must be followed by a call")
		}
		if p.at("(") {
			p.next()
			save := p.noStruct
			p.noStruct = false
			var args []Expr
			for !p.at(")") {
				args = append(args, p.expr())
				if !p.accept(",") {
					break
				}
			}
			p.noStruct = save
			p.expect(")")
			return &Call{base: b, Name: name, Args: args, TypeArgs: targs}
		}
		if p.at("{") && !p.noStruct && len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
			p.next()
			sl := &StructLit{base: b, Name: name}
			for !p.at("}") {
				fn := p.ident()
				p.expect(":")
				sl.Fields = append(sl.Fields, FieldInit{Pos: fn.Pos, Name: fn.Text, X: p.expr()})
				if !p.accept(",") {
					break
				}
			}
			p.expect("}")
			return sl
		}
		return &Ident{base: b, Name: name}
	case Op:
		switch t.Text {
		case "(":
			save := p.noStruct
			p.noStruct = false
			e := p.expr()
			p.noStruct = save
			p.expect(")")
			return e
		case "[":
			save := p.noStruct
			p.noStruct = false
			defer func() { p.noStruct = save }()
			if p.accept("]") {
				p.fail(t, "empty array literals are not allowed")
			}
			first := p.expr()
			if p.accept(";") {
				n := p.expr()
				p.expect("]")
				return &ArrayRep{base: b, Val: first, N: n}
			}
			al := &ArrayLit{base: b, Elems: []Expr{first}}
			for p.accept(",") {
				if p.at("]") {
					break
				}
				al.Elems = append(al.Elems, p.expr())
			}
			p.expect("]")
			return al
		}
	}
	desc := t.Text
	if t.Kind == EOF {
		desc = "end of file"
	}
	p.fail(t, "unexpected `%s` in expression", desc)
	return nil
}
