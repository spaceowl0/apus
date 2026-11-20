package syntax

import (
	"strings"
	"testing"

	"apus/internal/diag"
)

func parseOK(t *testing.T, src string) *Program {
	t.Helper()
	var p *Program
	if err := diag.Try(func() { p = Parse(src) }); err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	return p
}

func parseErr(t *testing.T, src string) *diag.Error {
	t.Helper()
	err := diag.Try(func() { Parse(src) })
	if err == nil {
		t.Fatalf("expected a parse error for %q", src)
	}
	return err
}

func TestLexer(t *testing.T) {
	toks := Lex(`let x = 0xFF_ff; // c
	a ++ b ..= <<= "q\n\"" /* x */ 12`)
	var kinds []string
	for _, tk := range toks {
		kinds = append(kinds, tk.Text)
	}
	got := strings.Join(kinds, "|")
	if !strings.Contains(got, "++") || !strings.Contains(got, "..=") {
		t.Errorf("multi-char operators not lexed: %s", got)
	}
	for _, tk := range toks {
		if tk.Kind == Str_ && tk.Text != "q\n\"" {
			t.Errorf("string escapes wrong: %q", tk.Text)
		}
	}
}

func TestLexerHexAndPositions(t *testing.T) {
	toks := Lex("  0x1F_FF\n  abc")
	if toks[0].Val.Int64() != 0x1FFF {
		t.Errorf("hex value = %v", toks[0].Val)
	}
	if toks[1].Pos.Line != 2 || toks[1].Pos.Col != 3 {
		t.Errorf("position = %v, want 2:3", toks[1].Pos)
	}
}

func TestLexerErrors(t *testing.T) {
	for _, src := range []string{`"abc`, "\"abc\n\"", `@`, `/* never closed`, `"bad \q escape"`} {
		if err := diag.Try(func() { Lex(src) }); err == nil || err.Code != "E0001" {
			t.Errorf("Lex(%q): want E0001, got %v", src, err)
		}
	}
}

func TestPrecedence(t *testing.T) {
	p := parseOK(t, `fn main() { let x = 1 + 2 * 3 << 1 & 7 == 3 || false && true; }`)
	let := p.Fns[0].Body[0].(*Let)
	top, ok := let.Init.(*Binary)
	if !ok || top.Op != "||" {
		t.Fatalf("top operator = %T %v, want ||", let.Init, top)
	}
	and := top.R.(*Binary)
	if and.Op != "&&" {
		t.Errorf("rhs of || should be &&, got %s", and.Op)
	}
	eq := top.L.(*Binary)
	if eq.Op != "==" {
		t.Errorf("lhs of || should be ==, got %s", eq.Op)
	}
}

func TestCastBindsTighterThanBinary(t *testing.T) {
	p := parseOK(t, `fn main() { let x = a as u64 + b as u64; }`)
	b := p.Fns[0].Body[0].(*Let).Init.(*Binary)
	if _, ok := b.L.(*Cast); !ok {
		t.Errorf("left operand should be a cast, got %T", b.L)
	}
	if _, ok := b.R.(*Cast); !ok {
		t.Errorf("right operand should be a cast, got %T", b.R)
	}
}

func TestStructLiteralAmbiguity(t *testing.T) {
	parseOK(t, `struct P { x: i32 } fn main() { if x == y { } let p = P { x: 1 }; while c bound(3) { } }`)
}

func TestNegativeLiteralFolding(t *testing.T) {
	p := parseOK(t, `fn main() { let a = -5; let b = - x; let c = 3 - -2; }`)
	if il, ok := p.Fns[0].Body[0].(*Let).Init.(*IntLit); !ok || il.V.Int64() != -5 {
		t.Error("-5 should fold to a negative literal")
	}
	if _, ok := p.Fns[0].Body[1].(*Let).Init.(*Unary); !ok {
		t.Error("-x should stay a unary expression")
	}
}

func TestCompoundAssignDesugars(t *testing.T) {
	p := parseOK(t, `fn main() { x += 1; a[i] *= 2; s.f -= 3; }`)
	for _, st := range p.Fns[0].Body {
		a := st.(*Assign)
		if b, ok := a.X.(*Binary); !ok || (b.Op != "+" && b.Op != "*" && b.Op != "-") {
			t.Errorf("compound assignment not desugared: %#v", a.X)
		}
	}
}

func TestNestedReturnDetection(t *testing.T) {
	p := parseOK(t, `fn a() -> i32 { return 1; } fn b(x: i32) -> i32 { if x > 0 { return 1; } return 2; }
	fn c() { for i in 0..3 { { return; } } }`)
	if p.Fns[0].NestedRet || !p.Fns[1].NestedRet || !p.Fns[2].NestedRet {
		t.Errorf("NestedRet = %v %v %v, want false true true", p.Fns[0].NestedRet, p.Fns[1].NestedRet, p.Fns[2].NestedRet)
	}
}

func TestItems(t *testing.T) {
	p := parseOK(t, `const A: u32 = 1; struct S { a: u8, b: [u32; 4], c: Str<8> } arena Heap(4096);
	pure fn f(x: u32 in 0..=10, y: i32 in -5..=5) -> u32 bound(rec = 9) io { return x; }`)
	if len(p.Consts) != 1 || len(p.Structs) != 1 || len(p.Arenas) != 1 || len(p.Fns) != 1 {
		t.Fatalf("items = %d/%d/%d/%d", len(p.Consts), len(p.Structs), len(p.Arenas), len(p.Fns))
	}
	f := p.Fns[0]
	if !f.Pure || !f.IO || f.BoundRec != 9 || f.Params[0].Hi.Int64() != 10 || f.Params[1].Lo.Int64() != -5 {
		t.Errorf("fn attributes parsed wrongly: %+v", f)
	}
	if p.Arenas[0].Size != 4096 {
		t.Error("arena size")
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{
		`fn main( { }`, `fn main() { let = 5; }`, `fn main() { while c { } }`, `fn main() { 1 + ; }`,
		`fn main() { let a = []; }`, `struct { }`, `fn main() { x + 1 = 2; }`, `arena A(0);`, `fn main() { for i in 0..3 }`,
		`garbage`, `fn main() { if x { }`,
	} {
		if err := parseErr(t, src); err.Code != "E0002" {
			t.Errorf("%q: code = %s, want E0002", src, err.Code)
		}
	}
}
