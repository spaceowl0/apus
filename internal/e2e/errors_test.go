package e2e

import (
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"apus/internal/driver"
	"apus/internal/ir"
	"apus/internal/peval"
)

// Every entry must be rejected at compile time with the given error code.
var errorCases = []struct {
	name, code, src string
	fuel            int
}{
	{"const overflow", "E0301", `fn main() -> i32 { return 2147483647 + 1; }`, 0},
	{"u32 sub underflow", "E0301", `fn main() io { let a: u32 = 3; let b: u32 = 5; io::println(str(a - b)); }`, 0},
	{"literal out of range", "E0301", `fn main() io { let x: u8 = 256; io::println(str(x)); }`, 0},
	{"negative unsigned literal", "E0301", `fn main() io { let x: u32 = -1; io::println(str(x)); }`, 0},
	{"checked cast const", "E0301", `fn main() io { let x: u32 = 300; let y = x as u8; io::println(str(y)); }`, 0},
	{"shift too large", "E0301", `fn main() io { let x: u32 = 1; io::println(str(x << 40)); }`, 0},
	{"division by zero const", "E0303", `fn main() io { let x = 5 / 0; io::println(str(x)); }`, 0},
	{"const index out of range", "E0304", `fn main() io { let a = [1, 2, 3]; io::println(str(a[5])); }`, 0},
	{"negative index", "E0304", `fn main() io { let a = [1, 2, 3]; io::println(str(a[-1])); }`, 0},
	{"arena over budget", "E0412", `arena A(100); fn main() { let p = alloc(A, 200); }`, 0},
	{"arena accumulates", "E0412", `arena A(100); fn main() { let p = alloc(A, 60); let q = alloc(A, 60); }`, 0},
	{"arena unbounded size", "E0412", `arena A(4096); fn main() io { let n = io::read_u32(); let p = alloc(A, n as u64); }`, 0},
	{"arena loop multiplies", "E0412", `arena A(1000); fn main() io { let n = clamp(io::read_u32(), 0, 10); for i in 0..n { let p = alloc(A, 200); } }`, 0},
	{"static not folding", "E0101", `fn main() io { let n = io::read_u32(); io::println(str(static n)); }`, 0},
	{"const depends on input", "E0101", `fn f() -> u32 io { return io::read_u32(); } const C: u32 = 1; fn main() io { io::println(str(static f())); }`, 0},
	{"unbounded recursion", "E0201", `pure fn f(n: u32) -> u32 { return f(n); } fn main() {}`, 0},
	{"recursion bound exceeded", "E0201", `pure fn f(n: u32) -> u32 bound(rec = 5) { if n == 0 { return 0; } return f(n - 1) + 1; } fn main() io { io::println(str(static f(10))); }`, 0},
	{"mutual recursion needs bound", "E0201", `pure fn a(n: u32) -> u32 { return b(n); } pure fn b(n: u32) -> u32 bound(rec = 3) { return a(n); } fn main() {}`, 0},
	{"loop exceeds static bound", "E0201", `fn main() { let mut i = 0; while i < 10 bound(3) { i += 1; } }`, 0},
	{"for with unbounded trip count", "E0201", `fn main() io { let n = io::read_u32(); for i in 0..n { } }`, 0},
	{"pure calls io", "E0102", `pure fn f() { io::println("x"); } fn main() io { f(); }`, 0},
	{"io from non-io fn", "E0102", `fn f() { io::println("x"); } fn main() io { f(); }`, 0},
	{"pure calls impure", "E0102", `fn g() -> u32 { return 1; } pure fn f() -> u32 { return g(); } fn main() { }`, 0},
	{"pure alloc", "E0102", `arena A(10); pure fn f() { let p = alloc(A, 1); } fn main() {}`, 0},
	{"type mismatch bool to u32", "E0305", `fn main() { let x: u32 = true; }`, 0},
	{"mismatched operands", "E0305", `fn main() { let a: u32 = 1; let b: i32 = 2; let c = a + b; }`, 0},
	{"if condition not bool", "E0305", `fn main() { if 1 { } }`, 0},
	{"return type mismatch", "E0305", `fn f() -> u32 { return true; } fn main() {}`, 0},
	{"string plus int", "E0302", `fn main() io { io::println("a" ++ 5); }`, 0},
	{"println of int", "E0305", `fn main() io { io::println(5); }`, 0},
	{"assign immutable", "E0310", `fn main() { let x = 1; x = 2; }`, 0},
	{"assign param", "E0310", `fn f(x: u32) { x = 2; } fn main() {}`, 0},
	{"refinement violated", "E0501", `fn f(n: u32 in 0..=10) -> u32 { return n; } fn main() io { let x = f(io::read_u32()); }`, 0},
	{"refinement const violated", "E0501", `fn f(n: u32 in 0..=10) -> u32 { return n; } fn main() { let x = f(11); }`, 0},
	{"unknown name", "E0004", `fn main() { let x = y; }`, 0},
	{"unknown function", "E0004", `fn main() { foo(); }`, 0},
	{"unknown type", "E0004", `fn main() { let x: Nope = 1; }`, 0},
	{"unknown field", "E0004", `struct P { x: i32 } fn main() { let p = P { x: 1 }; let y = p.z; }`, 0},
	{"missing struct field", "E0004", `struct P { x: i32, y: i32 } fn main() { let p = P { x: 1 }; }`, 0},
	{"duplicate fn", "E0004", `fn f() {} fn f() {} fn main() {}`, 0},
	{"unknown arena", "E0004", `fn main() { let p = alloc(Nope, 1); }`, 0},
	{"missing main", "E0003", `fn f() {}`, 0},
	{"main with params", "E0003", `fn main(x: u32) {}`, 0},
	{"missing return", "E0306", `fn f(x: u32) -> u32 { if x > 1 { return 1; } } fn main() {}`, 0},
	{"wrong arg count", "E0005", `fn f(a: u32) {} fn main() { f(1, 2); }`, 0},
	{"static_assert fails", "E0308", `fn main() { static_assert(1 + 1 == 3); }`, 0},
	{"require always fails", "E0308", `fn main() io { let x = clamp(io::read_u32(), 10, 20); let y = require(x, 100, 200); }`, 0},
	{"syntax error", "E0002", `fn main( { }`, 0},
	{"while without bound", "E0002", `fn main() { while true { } }`, 0},
	{"unterminated string", "E0001", "fn main() { io::println(\"abc); }", 0},
	{"bad character", "E0001", "fn main() { let x = 5 @ 3; }", 0},
	{"fuel exhausted", "E0103", `pure fn f(n: u32) -> u32 bound(rec = 100000) { if n == 0 { return 0; } return f(n - 1) + 1; } fn main() io { io::println(str(static f(50000))); }`, 1000},
	{"recursive struct", "E0004", `struct S { a: S } fn main() {}`, 0},
	{"empty array literal", "E0002", `fn main() { let a = []; }`, 0},
	{"non-exhaustive enum match", "E0309", `enum E { A, B, C } fn main() { let e = E::A; match e { E::A => {} E::B => {} } }`, 0},
	{"non-exhaustive int match", "E0309", `fn main() io { let x = io::read_i32(); match x { 0 => {} 1..=5 => {} } }`, 0},
	{"non-exhaustive bool match", "E0309", `fn main() io { let x = io::read_i32() > 0; match x { true => {} } }`, 0},
	{"break outside loop", "E0311", `fn main() { break; }`, 0},
	{"continue outside loop", "E0311", `fn main() { if true { continue; } }`, 0},
	{"unknown enum variant", "E0004", `enum E { A } fn main() { let e = E::Z; }`, 0},
	{"duplicate enum variant", "E0004", `enum E { A, A } fn main() {}`, 0},
	{"duplicate enum value", "E0004", `enum E { A = 1, B = 1 } fn main() {}`, 0},
	{"int to enum cast", "E0305", `enum E { A, B } fn main() { let e = 1 as E; }`, 0},
	{"enum arithmetic", "E0305", `enum E { A, B } fn main() { let e = E::A + E::B; }`, 0},
	{"enum compared with int", "E0305", `enum E { A, B } fn main() { let b = E::A == 0; }`, 0},
	{"wrong enum in pattern", "E0305", `enum E { A } enum F { X } fn main() { let e = E::A; match e { F::X => {} } }`, 0},
	{"int pattern for bool", "E0305", `fn main() io { let b = io::read_i32() > 0; match b { 1 => {} _ => {} } }`, 0},
	{"empty range pattern", "E0305", `fn main() io { let x = io::read_i32(); match x { 5..=1 => {} _ => {} } }`, 0},
	{"pattern out of range", "E0301", `fn main() io { let x = io::read_u32(); match x { 5000000000 => {} _ => {} } }`, 0},
	{"match arm types differ", "E0305", `fn main() io { let x = io::read_i32(); let y = match x { 0 => 1, _ => true }; }`, 0},
	{"assign to string element", "E0305", `fn main() { let mut s = "abc"; s[0] = 65; }`, 0},
	{"string ordering", "E0305", `fn main() { let b = "a" < "b"; }`, 0},
	{"string index out of range", "E0304", `fn main() io { io::println(str("abc"[5])); }`, 0},
	{"index on int", "E0305", `fn main() { let x = 5; let y = x[0]; }`, 0},
	{"bytes of runtime string", "E0101", `fn main() io { let s = io::read_line(4); let b = bytes(s); }`, 0},
	{"float modulo", "E0305", `fn main() { let a = 1.5 % 2.0; }`, 0},
	{"float bit operation", "E0305", `fn main() { let a = 1.5 & 2.0; }`, 0},
	{"float shift", "E0305", `fn main() { let a: f64 = 1.0; let b = a << 1; }`, 0},
	{"clamp on float", "E0305", `fn main() io { let a = clamp(io::read_f64(), 0.0, 1.0); }`, 0},
	{"match on float", "E0305", `fn main() io { let a = io::read_f64(); match a { _ => {} } }`, 0},
	{"f32 literal out of range", "E0301", `fn main() { let a: f32 = 1e39; }`, 0},
	{"inexact int literal as f32", "E0301", `fn main() { let a: f32 = 16777217; }`, 0},
	{"constant float to int out of range", "E0301", `fn main() { let a = 3000000000.5 as i32; }`, 0},
	{"constant NaN to int", "E0301", `fn main() { let n = 0.0 / 0.0; let a = n as i32; }`, 0},
	{"int plus float literal", "E0305", `fn main() { let a: i32 = 1; let b = a + 1.5; }`, 0},
	{"float as array index", "E0305", `fn main() { let a = [1, 2]; let i = 1.0; let x = a[i]; }`, 0},
	{"mixed float widths", "E0305", `fn main() { let a: f32 = 1.0; let b: f64 = 2.0; let c = a + b; }`, 0},
	{"bool to float cast", "E0305", `fn main() { let a = true as f64; }`, 0},
	{"float function on integer", "E0305", `fn main() { let n: i32 = 5; let a = sqrt(n); }`, 0},
	{"str of a struct", "E0305", `struct P { x: i32 } fn main() io { let p = P { x: 1 }; io::println(str(p)); }`, 0},
	{"abs on unsigned integer", "E0305", `fn main() { let n: u32 = 5; let a = abs(n); }`, 0},
	{"abs of signed integer is not a float", "E0305", `fn main() { let n: i32 = 5; let a: f64 = abs(n); }`, 0},
	{"cannot infer generic parameter", "E0313", `fn z<T>() -> T { return 0; } fn main() { let x = z(); }`, 0},
	{"conflicting generic types", "E0313", `fn m<T>(a: T, b: T) -> T { return a; } fn main() { let a: u8 = 1; let b: i32 = 2; let c = m(a, b); }`, 0},
	{"generic const argument too large", "E0313", `fn f<const N: u8>() -> u8 { return N; } fn main() { let x = f::<300>(); }`, 0},
	{"type arguments on non-generic fn", "E0313", `fn f() {} fn main() { f::<u8>(); }`, 0},
	{"too many generic arguments", "E0313", `fn f<T>(a: T) -> T { return a; } fn main() { let x = f::<u8, u8>(1); }`, 0},
	{"error inside an instantiation", "E0305", `fn neg<T>(a: T) -> T { return -a; } fn main() { let a: u32 = 1; let b = neg(a); }`, 0},
	{"generic main", "E0003", `fn main<T>() {}`, 0},
	{"embed without source dir", "E0309", `fn main() { let d = embed("x.txt"); }`, 0},
	{"embed non-literal", "E0309", `fn main() { let p = "x"; let d = embed(p); }`, 0},
	{"max_of on bool", "E0305", `fn main() { let m = max_of(bool); }`, 0},
	{"offsetof unknown field", "E0004", `struct S { a: u8 } fn main() { let o = offsetof(S, zz); }`, 0},
	{"sizeof unknown type", "E0004", `fn main() { let o = sizeof(Nope); }`, 0},
}

func TestCompileErrors(t *testing.T) {
	for _, tc := range errorCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			opt := peval.DefaultOptions()
			if tc.fuel > 0 {
				opt.Fuel = tc.fuel
			}
			_, err := driver.Compile(tc.src, opt)
			if err == nil {
				t.Fatalf("expected %s, but the program compiled", tc.code)
			}
			if err.Code != tc.code {
				t.Fatalf("expected %s, got %v", tc.code, err)
			}
			if err.Pos.Line < 1 {
				t.Errorf("diagnostic has no source position: %v", err)
			}
		})
	}
}

// Programs that must compile (guards against over-eager rejection).
var okCases = []struct{ name, src string }{
	{"shadowing", `fn main() io { let x = 1; { let x = 2; io::println(str(x)); } io::println(str(x)); }`},
	{"nested struct arrays", `struct A { v: [i32; 3] } struct B { a: A, n: i32 } fn main() io { let b = B { a: A { v: [1, 2, 3] }, n: 4 }; io::println(str(b.a.v[1] + b.n)); }`},
	{"string capacity subtyping", `fn f(s: Str<16>) io { io::println(s); } fn main() io { f("hi"); f(io::read_line(8)); }`},
	{"const refs const", `const A: u32 = 4; const B: u32 = A * 2 + 1; fn main() io { io::println(str(B)); }`},
	{"array len from const", `const N: usize = 3; fn main() io { let a = [7; N * 2]; io::println(str(len(a))); }`},
	{"sizeof const", `struct P { a: u8, b: u32 } fn main() io { io::println(str(sizeof(P))); }`},
	{"compound assign", `fn main() io { let mut x = 10; x += 5; x -= 3; x *= 2; x /= 4; x %= 4; io::println(str(x)); }`},
	{"bool ops", `fn main() io { let a = io::read_i32(); let b = a > 0 && a < 10 || a == 100; if !b { io::println("no"); } }`},
	{"unit function result", `fn f() io { io::println("x"); } fn main() io { f(); f(); }`},
	{"early return in loop (main)", `fn main() io { let n = clamp(io::read_u32(), 0, 5); for i in 0..n { if i == 3 { return; } io::println(str(i)); } }`},
	{"block comment", "/* hi */ fn main() { // c\n }"},
	{"hex and underscores", `fn main() io { let x: u32 = 0xFF_FF; io::println(str(x)); }`},
	{"print without newline", `fn main() io { io::print("a"); io::print("b"); }`},
	{"enum in struct and array", `enum C { R, G, B } struct P { c: C, n: u8 } fn main() io { let a = [C::R, C::G, C::B]; let p = P { c: C::B, n: 1 }; if p.c == a[2] { io::println("ok"); } }`},
	{"enum explicit values", `enum S { Ok = 0, NotFound = 404, Err = 500 } fn main() io { io::println(str(S::NotFound as u32)); }`},
	{"match with all forms", `fn main() io { let x = io::read_i32(); match x { 0 | 1 => {} 2..=9 => { } _ => { return; } } let y = match x { 0 => 1, _ => 2 }; io::println(str(y)); }`},
	{"break in match arm", `fn main() io { for i in 0..10 { match i { 5 => break, 3 => continue, _ => io::println(str(i)), } } }`},
	{"nested loops with break", `fn main() io { let n = clamp(io::read_u32(), 0, 4); for a in 0..n { for b in 0..n { if a == b { break; } } } }`},
	{"const string equality", `fn main() io { if "a" == "a" && "a" != "b" { io::println("y"); } }`},
	{"reflection in constants", `struct S { a: u32, b: u8 } fn main() io { let a = [0; sizeof(S)]; let b = [0; bits_of(u8)]; io::println(str(len(a) + len(b))); }`},
	{"embed-free bytes", `fn main() io { let b = bytes("hi"); io::println(str(b[0] as u32 + b[1] as u32)); }`},
	{"compare two runtime strings", `fn main() io { let a = io::read_line(4); let b = io::read_line(4); if a == b { io::println("eq"); } if a != "x" ++ b { io::println("ne"); } }`},
	{"float arithmetic and casts", `fn main() io { let a: f64 = 1.5; let b = a * 2.0 - 0.25; let n = b as i32; let c = n as f32; let d = c as f64; io::println(str(d + sqrt(b))); }`},
	{"float constants and arrays", `const PI: f64 = 3.14159; const T: [f32; 3] = [0.5, 1.5, -2.0]; struct P { x: f64, y: f32 } fn main() io { let p = P { x: PI, y: T[1] }; io::println(str(p.x) ++ str(p.y)); }`},
	{"float literals", `fn main() io { let a = 1e3; let b: f32 = 2.5e-2; let c = -0.5; let d = 1_000.25; io::println(str(a + c) ++ str(b) ++ str(d)); }`},
	{"integer literals as floats", `fn main() io { let a: f64 = 2; let b = a * 3 + 1; io::println(str(b)); }`},
	{"float comparison and min/max", `fn main() io { let a = io::read_f64(); if a < 1.0 || a >= 2.0 { io::println(str(min(a, 3.0) + max(a, -3.0))); } }`},
	{"generics over floats", `fn m<T>(a: T, b: T) -> T { if a > b { return a; } return b; } fn main() io { io::println(str(m(1.5, 2.5)) ++ str(m(3, 4))); }`},
	{"float in dynamic branches and loops", `fn main() io { let mut s = 0.0; let n = clamp(io::read_u32(), 0, 5); for i in 0..n { if (i as f64) > 1.5 { s += 1.5; } else { s -= 0.25; } } io::println(str(s)); }`},
	{"generic recursion", `pure fn fact<T>(n: T) -> T bound(rec = 20) { if n <= 1 { return 1; } return n * fact::<T>(n - 1); } fn main() io { io::println(str(static fact(5))); }`},
	{"generics call generics", `fn id<T>(a: T) -> T { return a; } fn twice<T>(a: T) -> T { return id(id(a)); } fn main() io { io::println(str(twice(7) + twice(1))); }`},
	{"generic over arrays and strings", `fn first<T, const N: usize>(a: [T; N]) -> T { return a[0]; } fn cap<const N: usize>(s: Str<N>) -> u64 { return N; } fn main() io { io::println(str(first([5, 6]) as u32) ++ str(cap("abc"))); }`},
	{"index and measure a built string", `fn main() io { let n = io::read_u32(); let s = "n=" ++ str(n); io::println(str(len(s)) ++ str(s[1] as u32)); }`},
	{"return inside runtime loop", `fn f(n: u32) -> u32 { for i in 0..n { if i * i > 50 { return i; } } return 0; } fn main() io { io::println(str(f(clamp(io::read_u32(), 0, 20)))); }`},
	{"return inside nested loops", `fn f(a: u32, b: u32) -> u32 { for i in 0..clamp(a, 0, 3) { for j in 0..clamp(b, 0, 4) { if i * j == a { return i * 10 + j; } } } return 99; } fn main() io { io::println(str(f(io::read_u32(), io::read_u32()))); }`},
	{"string index runtime", `fn main() io { let s = io::read_line(4); let i = clamp(io::read_u32(), 0, 3); io::println(str(s[i as u64])); }`},
}

func TestCompileOK(t *testing.T) {
	for _, tc := range okCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := driver.Compile(tc.src, peval.DefaultOptions())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDiagnosticsRender(t *testing.T) {
	_, err := driver.Compile("fn main() -> i32 {\n    return 2147483647 + 1;\n}\n", peval.DefaultOptions())
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Pos.Line != 2 || !strings.Contains(err.Msg, "overflow") {
		t.Errorf("unexpected diagnostic: %+v", err)
	}
}

func TestExamples(t *testing.T) {
	files, _ := filepath.Glob("../../examples/*.apus")
	if len(files) < 14 {
		t.Fatalf("expected example programs, found %d", len(files))
	}
	for _, f := range files {
		src, _ := os.ReadFile(f)
		if _, err := driver.Compile(string(src), peval.DefaultOptions()); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
	bad, _ := filepath.Glob("../../examples/errors/*.apus")
	if len(bad) < 5 {
		t.Fatalf("expected error examples, found %d", len(bad))
	}
	for _, f := range bad {
		src, _ := os.ReadFile(f)
		first := strings.SplitN(string(src), "\n", 2)[0]
		want := strings.TrimSpace(strings.TrimPrefix(first, "// expect:"))
		_, err := driver.Compile(string(src), peval.DefaultOptions())
		if err == nil || err.Code != want {
			t.Errorf("%s: want %s, got %v", filepath.Base(f), want, err)
		}
	}
}

func TestEmbedSandbox(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.bin"), []byte{1, 2, 3, 4}, 0o644)
	os.WriteFile(filepath.Join(dir, "empty.bin"), nil, 0o644)
	ok := func(src string) error {
		_, err := driver.CompileIn(src, dir, peval.DefaultOptions())
		if err == nil {
			return nil
		}
		return err
	}
	if err := ok(`fn main() io { let d = embed("a.bin"); io::println(str(len(d))); }`); err != nil {
		t.Fatalf("embed of an existing file failed: %v", err)
	}
	for name, src := range map[string]string{
		"missing":  `fn main() { let d = embed("nope.bin"); }`,
		"parent":   `fn main() { let d = embed("../a.bin"); }`,
		"absolute": `fn main() { let d = embed("/etc/passwd"); }`,
		"empty":    `fn main() { let d = embed("empty.bin"); }`,
		"dotdot":   `fn main() { let d = embed("sub/../../a.bin"); }`,
	} {
		err := ok(src)
		if err == nil || err.Error() == "" {
			t.Errorf("%s: embed must be rejected", name)
			continue
		}
		if e, isDiag := err.(interface{ Error() string }); !isDiag || !strings.Contains(e.Error(), "E0309") {
			t.Errorf("%s: want E0309, got %v", name, err)
		}
	}
}

const cfgProgram = `const DEBUG: bool = false;
const BUF: u32 = 8;
const LEVEL: u32 = 2;
fn main() io {
    static_assert(BUF >= 4, "BUF must be at least 4");
    static_print("building with BUF=" ++ str(BUF));
    let buf = [0; BUF];
    if DEBUG { io::println("debug on"); }
    match LEVEL { 0 => io::println("quiet"), 1 | 2 => io::println("normal"), _ => io::println("loud") }
    io::println("buffer length " ++ str(len(buf)));
}`

func TestBuildTimeDefines(t *testing.T) {
	run := func(defs map[string]int64) (string, *driver.Output, error) {
		m := map[string]*big.Int{}
		for k, v := range defs {
			m[k] = big.NewInt(v)
		}
		out, err := driver.CompileWith(cfgProgram, driver.Options{Defines: m, PE: peval.DefaultOptions()})
		if err != nil {
			return "", nil, err
		}
		var sb strings.Builder
		ir.Run(out.Res.Prog, strings.NewReader(""), &sb)
		return sb.String(), out, nil
	}
	got, out, err := run(nil)
	if err != nil || got != "normal\nbuffer length 8\n" {
		t.Fatalf("defaults: %q %v", got, err)
	}
	if len(out.Res.Notes) != 1 || !strings.Contains(out.Res.Notes[0], "BUF=8") {
		t.Errorf("static_print note missing: %v", out.Res.Notes)
	}
	got, _, err = run(map[string]int64{"DEBUG": 1, "BUF": 32, "LEVEL": 9})
	if err != nil || got != "debug on\nloud\nbuffer length 32\n" {
		t.Errorf("overridden: %q %v", got, err)
	}
	// overriding a constant must also change array lengths, which sema evaluates
	if _, _, err := run(map[string]int64{"BUF": 2}); err == nil || !strings.Contains(err.Error(), "BUF must be at least 4") {
		t.Errorf("static_assert message not reported: %v", err)
	}
	for name, defs := range map[string]map[string]int64{
		"unknown constant":  {"NOPE": 1},
		"does not fit":      {"BUF": 1 << 40},
		"negative unsigned": {"LEVEL": -1},
	} {
		if _, _, err := run(defs); err == nil || !strings.Contains(err.Error(), "E0312") {
			t.Errorf("%s: want E0312, got %v", name, err)
		}
	}
}
