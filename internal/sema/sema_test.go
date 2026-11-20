package sema_test

import (
	"sort"
	"strings"
	"testing"

	"apus/internal/diag"
	"apus/internal/sema"
	"apus/internal/syntax"
	"apus/internal/types"
)

func check(t *testing.T, src string) (*syntax.Program, *sema.Info) {
	t.Helper()
	var p *syntax.Program
	var info *sema.Info
	if err := diag.Try(func() { p = syntax.Parse(src); info = sema.Check(p) }); err != nil {
		t.Fatalf("check failed: %v", err)
	}
	return p, info
}

func letType(t *testing.T, src string) *types.Type {
	t.Helper()
	p, _ := check(t, "fn main() { "+src+" }")
	var last *types.Type
	for _, s := range p.Fns[0].Body {
		if l, ok := s.(*syntax.Let); ok {
			last = l.Ty // the binding under test is the last `let`
		}
	}
	if last == nil {
		t.Fatal("no let")
	}
	return last
}

func TestLiteralInference(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{`let x = 5;`, "i32"},
		{`let x: u8 = 5;`, "u8"},
		{`let x: u64 = 1 + 2 * 3;`, "u64"},
		{`let a: u16 = 1; let x = a + 2;`, "u16"},
		{`let a: u16 = 1; let x = 2 + a;`, "u16"},
		{`let x = 1 < 2;`, "bool"},
		{`let x = [1, 2, 3];`, "[i32; 3]"},
		{`let x: [u8; 2] = [1, 2];`, "[u8; 2]"},
		{`let x = [0; 5];`, "[i32; 5]"},
		{`let x = clamp(5, 0, 10);`, "i32"},
		{`let x: u8 = clamp(5, 0, 10);`, "u8"},
		{`let a: i64 = 1; let x = min(a, 2);`, "i64"},
		{`let x = 7 as u64;`, "u64"},
		{`let x = "hello";`, "Str<5>"},
		{`let x = "ab" ++ "cde";`, "Str<5>"},
		{`let x = str(5);`, "Str<11>"},
		{`let x = sizeof(u32);`, "u64"},
	}
	for _, c := range cases {
		if got := letType(t, c.src).String(); got != c.want {
			t.Errorf("%s: type = %s, want %s", c.src, got, c.want)
		}
	}
}

func TestStructAndFieldTypes(t *testing.T) {
	p, info := check(t, `struct A { a: u8, b: [i32; 2] } fn main() { let s = A { a: 1, b: [1, 2] }; let y = s.b[1]; let z = s.a; }`)
	if info.Structs["A"].Size() != 12 {
		t.Errorf("struct A size = %d", info.Structs["A"].Size())
	}
	body := p.Fns[0].Body
	if body[1].(*syntax.Let).Ty.String() != "i32" || body[2].(*syntax.Let).Ty.String() != "u8" {
		t.Errorf("field types wrong")
	}
}

func TestEffectRules(t *testing.T) {
	ok := []string{
		`pure fn f(x: u32) -> u32 { return x + 1; } fn main() { let y = f(2); }`,
		`fn g() io { io::println("x"); } fn main() io { g(); }`,
		`pure fn f() -> u32 { return 1; } pure fn g() -> u32 { return f(); } fn main() {}`,
	}
	for _, s := range ok {
		check(t, s)
	}
}

func TestRecursionAnalysis(t *testing.T) {
	check(t, `pure fn f(n: u32) -> u32 bound(rec = 5) { if n == 0 { return 0; } return f(n - 1); } fn main() {}`)
	for _, src := range []string{
		`pure fn f(n: u32) -> u32 { return f(n); } fn main() {}`,
		`fn a() { b(); } fn b() bound(rec = 2) { a(); } fn main() {}`,
	} {
		err := diag.Try(func() { sema.Check(syntax.Parse(src)) })
		if err == nil || err.Code != "E0201" {
			t.Errorf("want E0201 for %q, got %v", src, err)
		}
	}
}

func TestMainSignature(t *testing.T) {
	check(t, `fn main() -> u8 { return 3; }`)
	check(t, `fn main() io { }`)
	for _, src := range []string{`fn main() -> bool { return true; }`, `fn main(a: u32) {}`, `fn other() {}`} {
		if err := diag.Try(func() { sema.Check(syntax.Parse(src)) }); err == nil || err.Code != "E0003" {
			t.Errorf("%q: want E0003, got %v", src, err)
		}
	}
}

func TestEnumsAndMatchTyping(t *testing.T) {
	p, info := check(t, `enum Color { Red, Green = 5, Blue }
	fn main() io { let c = Color::Blue; let n = match c { Color::Red => 1, Color::Green => 2, Color::Blue => 3 }; let k = c as u32; }`)
	e := info.Enums["Color"]
	if e == nil || len(e.Variants) != 3 || e.Variants[2].Value != 6 || e.Size() != 4 {
		t.Fatalf("enum not built correctly: %+v", e)
	}
	body := p.Fns[0].Body
	if got := body[0].(*syntax.Let).Ty.String(); got != "Color" {
		t.Errorf("variant type = %s", got)
	}
	if got := body[1].(*syntax.Let).Ty.String(); got != "i32" {
		t.Errorf("match result type = %s, want i32", got)
	}
	if got := body[2].(*syntax.Let).Ty.String(); got != "u32" {
		t.Errorf("cast type = %s", got)
	}
}

func TestMatchExhaustivenessByRanges(t *testing.T) {
	ok := []string{
		`fn main() io { let x = io::read_u32(); match x { 0..=9 => {} 10..=max_of(u32) => {} } }`,
		`fn main() io { let x = io::read_i32(); match x { min_of(i32)..=0 => {} 1..=max_of(i32) => {} } }`,
		`fn main() io { let b = io::read_i32() > 0; match b { true => {} false => {} } }`,
		`fn main() io { let x = io::read_i32(); match x { 1 => {} _ => {} } }`,
	}
	for _, s := range ok {
		check(t, s)
	}
	bad := []string{
		`fn main() io { let x = io::read_u32(); match x { 0..=9 => {} 11..=max_of(u32) => {} } }`,
		`fn main() io { let x = io::read_u32(); match x { 0..=9 => {} 10..=4294967294 => {} } }`,
	}
	for _, s := range bad {
		if err := diag.Try(func() { sema.Check(syntax.Parse(s)) }); err == nil || err.Code != "E0309" {
			t.Errorf("want E0309 for %q, got %v", s, err)
		}
	}
}

func TestGenericInstancesAreMemoized(t *testing.T) {
	_, info := check(t, `fn max2<T>(a: T, b: T) -> T { if a > b { return a; } return b; }
	fn main() io {
	    let a: u8 = 1; let b: u8 = 2; let x = io::read_i32(); let y = io::read_i32();
	    let p = max2(x, y); let q = max2(y, x); let r = max2(a, b); let s = max2(3, 4);
	}`)
	var names []string
	for n, f := range info.Fns {
		if strings.HasPrefix(n, "max2<") {
			names = append(names, n)
			if f.Ret == nil || len(f.Generics) != 0 {
				t.Errorf("instance %s is not fully resolved", n)
			}
		}
	}
	sort.Strings(names)
	if got := strings.Join(names, " "); got != "max2<i32> max2<u8>" {
		t.Errorf("instances = %q, want one i32 and one u8 (untyped literals default to i32)", got)
	}
}

func TestConstGenericInference(t *testing.T) {
	_, info := check(t, `fn len2<const N: usize>(a: [u8; N]) -> u64 { return N; }
	fn cap<const N: usize>(s: Str<N>) -> u64 { return N; }
	fn main() io { let a: [u8; 5] = [0; 5]; let b: [u8; 2] = [1, 2]; io::println(str(len2(a) + len2(b) + cap("hello"))); }`)
	for _, want := range []string{"len2<5>", "len2<2>", "cap<5>"} {
		f := info.Fns[want]
		if f == nil {
			t.Errorf("missing instance %s", want)
			continue
		}
		if f.Ret.String() != "u64" {
			t.Errorf("%s returns %s", want, f.Ret)
		}
	}
	if f := info.Fns["len2<5>"]; f != nil && f.Params[0].Ty.String() != "[u8; 5]" {
		t.Errorf("parameter type = %s, want [u8; 5]", f.Params[0].Ty)
	}
}

func TestGenericErrorsMentionTheInstance(t *testing.T) {
	err := diag.Try(func() {
		sema.Check(syntax.Parse(`fn neg<T>(a: T) -> T { return -a; } fn main() { let a: u32 = 1; let b = neg(a); }`))
	})
	if err == nil || !strings.Contains(err.Msg, "neg<u32>") {
		t.Fatalf("want an error naming the failing instantiation, got %v", err)
	}
}

func TestFloatTyping(t *testing.T) {
	cases := []struct{ src, want string }{
		{`let x = 1.5;`, "f64"},
		{`let x: f32 = 1.5;`, "f32"},
		{`let x: f32 = 2;`, "f32"},
		{`let a: f32 = 1.0; let x = a * 2.5 + 1;`, "f32"},
		{`let x = 1.5 + 2;`, "f64"},
		{`let x = 1.5 < 2.0;`, "bool"},
		{`let x = [1.5, 2.5];`, "[f64; 2]"},
		{`let x: [f32; 2] = [1.5, 2.5];`, "[f32; 2]"},
		{`let x = [0.5; 4];`, "[f64; 4]"},
		{`let x = 3 as f32;`, "f32"},
		{`let a: f64 = 2.5; let x = a as i32;`, "i32"},
		{`let a: f32 = 2.5; let x = a as f64;`, "f64"},
		{`let x = sqrt(2.0);`, "f64"},
		{`let x: f32 = sqrt(2.0);`, "f32"},
		{`let a: f32 = 4.0; let x = floor(a);`, "f32"},
		{`let x = is_nan(1.0);`, "bool"},
		{`let a: f32 = 1.0; let x = min(a, 2.0);`, "f32"},
		{`let x = str(2.5);`, "Str<24>"},
		{`let x = -2.5;`, "f64"},
		{`let x = sizeof(f32) + sizeof(f64);`, "u64"},
	}
	for _, c := range cases {
		if got := letType(t, c.src).String(); got != c.want {
			t.Errorf("%s: type = %s, want %s", c.src, got, c.want)
		}
	}
}

func TestFloatLayout(t *testing.T) {
	_, info := check(t, `struct V { a: f32, b: f64, c: u8 } fn main() {}`)
	s := info.Structs["V"]
	if s.Size() != 24 || s.Align() != 8 || s.Offset(1) != 8 {
		t.Errorf("struct with floats: size %d align %d offset(b) %d, want 24/8/8", s.Size(), s.Align(), s.Offset(1))
	}
}
