package peval_test

import (
	"strings"
	"testing"

	"apus/internal/ir"
)

func TestNarrowingRemovesDivisionCheck(t *testing.T) {
	r := compile(t, `fn main() io { let x = io::read_i32(); if x > 0 { io::println(str(100 / x)); } }`)
	if strings.Contains(text(r), "div!") {
		t.Errorf("division inside `x > 0` needs no zero check:\n%s", text(r))
	}
	r = compile(t, `fn main() io { let x = io::read_i32(); io::println(str(100 / x)); }`)
	if !strings.Contains(text(r), "div!") {
		t.Errorf("unguarded division must stay checked:\n%s", text(r))
	}
}

func TestNarrowingDoesNotLeakOutOfBranch(t *testing.T) {
	r := compile(t, `fn main() io { let x = io::read_i32(); if x > 0 { io::println("pos"); } io::println(str(100 / x)); }`)
	if !strings.Contains(text(r), "div!") {
		t.Errorf("knowledge from a branch must not outlive it:\n%s", text(r))
	}
}

func TestGuardClauseNarrowsContinuation(t *testing.T) {
	r := compile(t, `fn main() io { let x = io::read_i32(); if x < 0 { return; } let y = x as u32; io::println(str(y)); }`)
	if strings.Contains(text(r), "cast ") {
		t.Errorf("after `if x < 0 {{ return; }}` the cast is free:\n%s", text(r))
	}
}

func TestShortCircuitNarrowing(t *testing.T) {
	r := compile(t, `fn main() io { let x = io::read_i32(); if x != 0 && 100 / x > 1 { io::println("y"); } if x == 0 || 100 / x > 1 { io::println("z"); } }`)
	if strings.Contains(text(r), "div!") {
		t.Errorf("right operands of && / || are only evaluated when safe:\n%s", text(r))
	}
}

func TestNarrowingInsideWhileBody(t *testing.T) {
	r := compile(t, `const T: [u32; 10] = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10];
	fn main() io { let mut i: u32 = 0; let mut s: u32 = 0; while i < 10 bound(10) { s += T[i as u64]; i += 1; } io::println(str(s)); }`)
	if strings.Contains(text(r), "tload t0[v") && strings.Contains(text(r), "] !") {
		t.Errorf("loop condition proves the index in range:\n%s", text(r))
	}
}

func TestMatchOnConstantFolds(t *testing.T) {
	r := compile(t, `enum M { A, B, C } const MODE: M = M::B;
	fn main() io { match MODE { M::A => io::println("a"), M::B => io::println("b"), M::C => io::println("c") } }`)
	got := text(r)
	if strings.Contains(got, "if ") || strings.Contains(got, `"a`) || strings.Contains(got, `"c`) || r.Stats.ResidualInsts != 2 {
		t.Errorf("constant scrutinee must select one arm:\n%s", got)
	}
}

func TestMatchRangesNarrowInsideArms(t *testing.T) {
	r := compile(t, `const T: [u32; 4] = [10, 20, 30, 40];
	fn main() io { let x = io::read_i32(); match x { 0..=3 => io::println(str(T[x as u64])), _ => io::println("out") } }`)
	if strings.Contains(text(r), "] !") {
		t.Errorf("inside `0..=3` the table index is proven:\n%s", text(r))
	}
}

func TestStringsAtCompileTime(t *testing.T) {
	r := compile(t, `fn main() io { let s = "hello"; let mut n: u32 = 0; for i in 0..len(s) { if s[i as u64] == 108 { n += 1; } } io::println(str(n)); }`)
	if r.Stats.ResidualInsts != 2 || !strings.Contains(text(r), `"2\n"`) {
		t.Errorf("counting letters in a literal must fold completely:\n%s", text(r))
	}
}

func TestConstTablesAreRodata(t *testing.T) {
	r := compile(t, `fn main() io { let t = [5, 6, 7, 8]; let i = clamp(io::read_u32(), 0, 3); io::println(str(t[i as u64])); }`)
	if len(r.Prog.Tables) != 1 || len(r.Prog.Arrs) != 0 || r.Prog.RodataBytes() != 16 {
		t.Errorf("want one 16-byte rodata table and no stack array:\n%s", text(r))
	}
}

func TestEmbedBecomesTable(t *testing.T) {
	r := compile(t, `fn main() io { let t = bytes("lookup"); let i = clamp(io::read_u32(), 0, 5); io::println(str(t[i as u64])); }`)
	if len(r.Prog.Tables) != 1 || r.Prog.Tables[0].Ty != (ir.IntTy{Bits: 8}) {
		t.Errorf("bytes() table should be u8 rodata:\n%s", text(r))
	}
}

func TestReflectionFolds(t *testing.T) {
	r := compile(t, `struct S { a: u8, b: u64, c: u16 }
	fn main() io { io::println(str(sizeof(S)) ++ "," ++ str(offsetof(S, b)) ++ "," ++ str(offsetof(S, c)) ++ "," ++ str(alignof(S)) ++ "," ++ str(max_of(i8)) ++ "," ++ str(min_of(i8)) ++ "," ++ str(bits_of(u16))); }`)
	if !strings.Contains(text(r), `"24,8,16,8,127,-128,16\n"`) {
		t.Errorf("reflection values wrong:\n%s", text(r))
	}
}

func TestBreakInRuntimeLoopUsesFlag(t *testing.T) {
	r := compile(t, `fn main() io { let n = clamp(io::read_u32(), 0, 50); let mut s: u32 = 0;
	for i in 0..n { if s > 100 { break; } s += i; } io::println(str(s)); }`)
	var out strings.Builder
	ir.Run(r.Prog, strings.NewReader("50\n"), &out)
	if out.String() != "105\n" { // 0+1+...+14 = 105 > 100 stops the loop
		t.Errorf("got %q\n%s", out.String(), text(r))
	}
}

func TestUnrolledLoopWithRuntimeBreak(t *testing.T) {
	r := compile(t, `fn main() io { let lim = io::read_i32(); let mut s = 0;
	for i in 0..5 { if i * 10 > lim { break; } s += i; } io::println(str(s)); }`)
	for in, want := range map[string]string{"100\n": "10\n", "25\n": "3\n", "-1\n": "0\n", "0\n": "0\n"} {
		var out strings.Builder
		ir.Run(r.Prog, strings.NewReader(in), &out)
		if out.String() != want {
			t.Errorf("lim=%q: got %q, want %q", in, out.String(), want)
		}
	}
}

func TestEnumLayoutAndRange(t *testing.T) {
	r := compile(t, `enum E { A = 3, B = 10 } struct S { e: E, b: u8 }
	fn main() io { io::println(str(sizeof(E)) ++ " " ++ str(sizeof(S)) ++ " " ++ str(E::B as u32 - E::A as u32)); }`)
	if !strings.Contains(text(r), `"4 8 7\n"`) {
		t.Errorf("enum layout/arith wrong:\n%s", text(r))
	}
}

func TestFloatFoldingMatchesIEEE(t *testing.T) {
	r := compile(t, `fn main() io {
	    let a: f32 = 0.1; let s = a + a + a;
	    let q = 1.0 / 3.0;
	    io::println(str(s) ++ " " ++ str(q * 3.0) ++ " " ++ str(sqrt(2.0)) ++ " " ++ str(10 as f64 / 4 as f64) ++ " " ++ str(1.0 / 0.0) ++ " " ++ str(-7.5 as i32));
	}`)
	if r.Stats.ResidualInsts != 2 || !strings.Contains(text(r), `"0.300000 1.000000 1.414214 2.500000 inf -7\n"`) {
		t.Errorf("float expressions with constant operands must fold completely:\n%s", text(r))
	}
}

func TestFloatCastsAreCheckedAtRuntime(t *testing.T) {
	r := compile(t, `fn main() io { let x = io::read_f64(); io::println(str(x as i32)); }`)
	if !strings.Contains(text(r), "cvt! f64 -> i32") {
		t.Errorf("a float to int cast of a runtime value must trap when out of range:\n%s", text(r))
	}
	r = compile(t, `fn main() io { let n = io::read_i32(); io::println(str(n as f64 * 0.5)); }`)
	if strings.Contains(text(r), "cvt!") {
		t.Errorf("int to float never traps:\n%s", text(r))
	}
}

func TestFloatTablesAreRodata(t *testing.T) {
	r := compile(t, `const T: [f64; 4] = [0.5, 0.25, 0.125, 0.0625];
	fn main() io { let i = clamp(io::read_u32(), 0, 3); io::println(str(T[i as u64] * 2.0)); }`)
	if len(r.Prog.Tables) != 1 || r.Prog.Tables[0].Ty != (ir.IntTy{Bits: 64}) || r.Prog.RodataBytes() != 32 {
		t.Errorf("float tables are 64-bit read-only data:\n%s", text(r))
	}
	if r.Prog.Tables[0].Vals[0] != ir.BitsOf64(0.5) {
		t.Errorf("table holds IEEE bit patterns, got %#x", r.Prog.Tables[0].Vals[0])
	}
}

func TestFloatLoopsRunAtCompileTime(t *testing.T) {
	r := compile(t, `pure fn harmonic(n: u32) -> f64 { let mut s = 0.0; for i in 1..n { s += 1.0 / (i as f64); } return s; }
	fn main() io { io::println(str(static harmonic(1001))); }`)
	if r.Stats.ResidualInsts != 2 || !strings.Contains(text(r), `"7.485471\n"`) {
		t.Errorf("float loop with constant bound must fold (H_1000 = 7.485471):\n%s", text(r))
	}
}
