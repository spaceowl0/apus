package peval_test

import (
	"strings"
	"testing"

	"apus/internal/driver"
	"apus/internal/ir"
	"apus/internal/peval"
)

func compile(t *testing.T, src string) *peval.Result {
	t.Helper()
	return compileOpt(t, src, peval.DefaultOptions())
}

func compileOpt(t *testing.T, src string, opt peval.Options) *peval.Result {
	t.Helper()
	out, err := driver.Compile(src, opt)
	if err != nil {
		t.Fatalf("compile error: %v\n%s", err, src)
	}
	return out.Res
}

func text(r *peval.Result) string { return ir.Format(r.Prog) }

func TestStaticStringsCollapse(t *testing.T) {
	r := compile(t, `const A: StaticStr = "foo"; const N: u32 = 6 * 7;
	fn main() io { io::println(A ++ "-" ++ str(N) ++ "-" ++ "bar"); }`)
	if r.Stats.ResidualInsts != 2 { // write + ret
		t.Errorf("want 2 instructions, got %d:\n%s", r.Stats.ResidualInsts, text(r))
	}
	if !strings.Contains(text(r), `"foo-42-bar\n"`) {
		t.Errorf("static pieces not merged:\n%s", text(r))
	}
}

func TestRopeKeepsOnlyDynamicHoles(t *testing.T) {
	r := compile(t, `fn main() io { let n = io::read_line(8); io::println("a" ++ "b" ++ n ++ "c" ++ "d"); }`)
	got := text(r)
	for _, want := range []string{`write "ab"`, `write b0[:v0]`, `write "cd\n"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestDeadBranchesAndCondsRemoved(t *testing.T) {
	r := compile(t, `const DEBUG: bool = false;
	fn main() io { if DEBUG { io::println("dbg"); } if 1 + 1 == 3 { io::println("never"); } else { io::println("yes"); } }`)
	if strings.Contains(text(r), "dbg") || strings.Contains(text(r), "never") {
		t.Errorf("dead branches survived:\n%s", text(r))
	}
	if r.Stats.BranchesRemoved != 2 {
		t.Errorf("branches removed = %d, want 2", r.Stats.BranchesRemoved)
	}
}

func TestRangeProvesBranchDead(t *testing.T) {
	r := compile(t, `fn main() io { let n = clamp(io::read_u32(), 0, 10); if n > 20 { io::println("never"); } io::println("ok"); }`)
	if strings.Contains(text(r), "never") || strings.Contains(text(r), "gt.") {
		t.Errorf("range analysis failed to remove the comparison:\n%s", text(r))
	}
}

func TestRangeRemovesOverflowChecks(t *testing.T) {
	r := compile(t, `fn main() io { let a = clamp(io::read_u32(), 0, 1000); let b = clamp(io::read_u32(), 0, 1000);
	io::println(str(a * b + 5)); }`)
	if strings.Contains(text(r), "!.") {
		t.Errorf("provably safe arithmetic should be unchecked:\n%s", text(r))
	}
	r = compile(t, `fn main() io { let a = io::read_u32(); let b = io::read_u32(); io::println(str(a * b)); }`)
	if !strings.Contains(text(r), "mul!.u32") {
		t.Errorf("unbounded multiplication must stay checked:\n%s", text(r))
	}
}

func TestCastCheckRemovedWhenInRange(t *testing.T) {
	r := compile(t, `fn main() io { let x = clamp(io::read_u32(), 0, 200); let y = x as u8; io::println(str(y)); }`)
	if strings.Contains(text(r), "cast ") {
		t.Errorf("cast should be free:\n%s", text(r))
	}
	r = compile(t, `fn main() io { let x = io::read_u32(); let y = x as u8; io::println(str(y)); }`)
	if !strings.Contains(text(r), "cast u32 -> u8") {
		t.Errorf("narrowing cast must be checked:\n%s", text(r))
	}
}

func TestIndexCheckEliminatedByRange(t *testing.T) {
	r := compile(t, `const T: [u32; 8] = [1, 2, 3, 4, 5, 6, 7, 8];
	fn main() io { let i = clamp(io::read_u32(), 0, 7); io::println(str(T[i as u64])); }`)
	if strings.Contains(text(r), "] !") || strings.Contains(text(r), "]!") {
		t.Errorf("bounds check should be proven away:\n%s", text(r))
	}
	r = compile(t, `const T: [u32; 8] = [1, 2, 3, 4, 5, 6, 7, 8];
	fn main() io { let i = io::read_u32(); io::println(str(T[i as u64])); }`)
	if !strings.Contains(text(r), "!") {
		t.Errorf("unbounded index must be checked:\n%s", text(r))
	}
}

func TestConstTablesAreShared(t *testing.T) {
	r := compile(t, `const T: [u32; 4] = [9, 8, 7, 6];
	fn main() io { let i = clamp(io::read_u32(), 0, 3); io::println(str(T[i as u64])); io::println(str(T[i as u64] + 1)); }`)
	if len(r.Prog.Tables) != 1 || len(r.Prog.Arrs) != 0 {
		t.Errorf("a constant table read twice should be one read-only table and no stack array, got %d tables, %d arrays", len(r.Prog.Tables), len(r.Prog.Arrs))
	}
}

func TestMemoizedPureCalls(t *testing.T) {
	r := compile(t, `pure fn fib(n: u32) -> u64 bound(rec = 93) { if n < 2 { return n as u64; } return fib(n - 1) + fib(n - 2); }
	fn main() -> u64 { return static fib(80); }`)
	if r.Stats.ResidualInsts != 1 || !strings.Contains(text(r), "ret 23416728348467685") {
		t.Errorf("fib(80) not folded:\n%s", text(r))
	}
	if r.Stats.PureCallsFolded > 100 || r.Stats.MemoHits < 70 {
		t.Errorf("memoization ineffective: folded=%d hits=%d", r.Stats.PureCallsFolded, r.Stats.MemoHits)
	}
}

func TestUnrollPolicy(t *testing.T) {
	src := `fn main() io { let mut s: u32 = 0; for i in 0..100 { s += i as u32; } io::println(str(s)); }`
	r := compileOpt(t, src, peval.Options{Fuel: 1_000_000, UnrollLimit: 256})
	if r.Stats.DynLoops != 0 || r.Stats.ResidualInsts > 3 {
		t.Errorf("loop below the unroll limit must fold completely:\n%s", text(r))
	}
	r = compileOpt(t, src, peval.Options{Fuel: 1_000_000, UnrollLimit: 10})
	if r.Stats.DynLoops != 1 || !strings.Contains(text(r), "loop") {
		t.Errorf("loop above the unroll limit must stay a loop:\n%s", text(r))
	}
}

func TestLoopVariantsKeepSemantics(t *testing.T) {
	src := `fn main() io { let mut s: u64 = 0; for i in 0..1000 { s += i as u64; } io::println(str(s)); }`
	r := compile(t, src)
	if r.Stats.DynLoops != 1 {
		t.Errorf("1000 iterations should stay a loop")
	}
	var out strings.Builder
	ir.Run(r.Prog, strings.NewReader(""), &out)
	if out.String() != "499500\n" {
		t.Errorf("got %q", out.String())
	}
}

func TestDeterministicOutput(t *testing.T) {
	src := `struct P { x: i32, y: i32 }
	fn main() io { let mut a = io::read_i32(); let mut b = io::read_i32(); let mut c = 0; let mut p = P { x: 1, y: 2 };
	for i in 0..3 { if a > b { a = a - 1; c += 1; p.x += 1; } else { b = b - 1; p.y += 2; } }
	io::println(str(a) ++ str(b) ++ str(c) ++ str(p.x) ++ str(p.y)); }`
	first := text(compile(t, src))
	for i := 0; i < 8; i++ {
		if again := text(compile(t, src)); again != first {
			t.Fatalf("compilation is not deterministic:\n%s\n--- vs ---\n%s", first, again)
		}
	}
}

func TestIdentitiesProduceNoCode(t *testing.T) {
	r := compile(t, `fn main() io { let x = io::read_i32(); let y = (x + 0) * 1 - 0; io::println(str(y)); io::println(str(x * 0)); }`)
	if strings.Contains(text(r), "add") || strings.Contains(text(r), "mul") || strings.Contains(text(r), "sub") {
		t.Errorf("identities should vanish:\n%s", text(r))
	}
}

func TestPartiallyStaticStruct(t *testing.T) {
	r := compile(t, `struct P { k: u32, v: u32 }
	fn main() io { let n = io::read_u32(); let p = P { k: 10 * 10, v: n }; io::println(str(p.k + 1) ++ " " ++ str(p.v)); }`)
	if !strings.Contains(text(r), `"101 "`) {
		t.Errorf("static field should fold through the struct:\n%s", text(r))
	}
}

func TestAlwaysTrappingOpWarnsAndTraps(t *testing.T) {
	r := compile(t, `fn main() io { let x = clamp(io::read_u32(), 0, 5); if x > 2 { let y = x - 100; io::println(str(y)); } }`)
	if len(r.Warnings) == 0 || !strings.Contains(text(r), "trap arithmetic overflow") {
		t.Errorf("expected a warning and an explicit trap; warnings=%v\n%s", r.Warnings, text(r))
	}
}

func TestArenaAccounting(t *testing.T) {
	r := compile(t, `arena A(1000);
	fn main() io { region A { let p = alloc(A, 300); region A { let q = alloc(A, 400); } let r = alloc(A, 100); } let s = alloc(A, 50); }`)
	if r.Arenas[0].HighWater != 700 {
		t.Errorf("high-water = %d, want 700", r.Arenas[0].HighWater)
	}
}

func TestDynamicReturnInlining(t *testing.T) {
	r := compile(t, `fn sign(x: i32) -> i32 { if x < 0 { return -1; } if x == 0 { return 0; } return 1; }
	fn main() io { let a = io::read_i32(); io::println(str(sign(a)) ++ str(sign(a - 1))); }`)
	var out strings.Builder
	ir.Run(r.Prog, strings.NewReader("5\n"), &out)
	if out.String() != "11\n" {
		t.Errorf("got %q", out.String())
	}
}

func TestStatsAreReported(t *testing.T) {
	r := compile(t, `fn main() io { for i in 0..4 { io::println(str(i)); } }`)
	if r.Stats.UnrolledIters != 4 || r.Stats.FuelUsed <= 0 || r.Stats.FoldedOps == 0 {
		t.Errorf("unexpected stats: %+v", r.Stats)
	}
	if r.Stats.InstsBeforeOpt < r.Stats.ResidualInsts {
		t.Errorf("optimizer cannot add instructions: %+v", r.Stats)
	}
}

// The unroll limit is a code-size policy for runtime code; inside a static
// (compile-time) evaluation loops must always run to completion.
func TestStaticLoopsIgnoreUnrollLimit(t *testing.T) {
	src := `pure fn tri(n: u32) -> u64 { let mut s: u64 = 0; for i in 0..n { s += i as u64; } return s; }
	fn main() io { io::println(str(static tri(5000))); }`
	r := compileOpt(t, src, peval.Options{Fuel: 10_000_000, UnrollLimit: 8})
	if r.Stats.DynLoops != 0 || !strings.Contains(text(r), "12497500") {
		t.Errorf("static evaluation must fold 5000 iterations regardless of the unroll limit:\n%s", text(r))
	}
	// the same loop on runtime data stays a loop under the same limit
	r = compileOpt(t, `fn main() io { let n = clamp(io::read_u32(), 0, 5000); let mut s: u64 = 0; for i in 0..n { s += i as u64; } io::println(str(s)); }`,
		peval.Options{Fuel: 10_000_000, UnrollLimit: 8})
	if r.Stats.DynLoops != 1 {
		t.Errorf("runtime loop should stay residual")
	}
}

func TestFuelBoundsStaticEvaluation(t *testing.T) {
	_, err := driver.Compile(`pure fn spin(n: u32) -> u64 { let mut s: u64 = 0; for i in 0..n { s += 1; } return s; }
	fn main() io { io::println(str(static spin(100000000))); }`, peval.Options{Fuel: 100_000, UnrollLimit: 256})
	if err == nil || err.Code != "E0103" {
		t.Fatalf("want E0103, got %v", err)
	}
}
