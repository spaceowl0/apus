package llvm_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"apus/internal/driver"
	"apus/internal/llvm"
	"apus/internal/peval"
)

func emit(t *testing.T, src, triple string) string {
	t.Helper()
	out, err := driver.Compile(src, peval.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return llvm.Emit(out.Res.Prog, triple)
}

func TestModuleShape(t *testing.T) {
	m := emit(t, `fn main() -> u64 { return 7; }`, "x86_64-unknown-linux-gnu")
	for _, want := range []string{`target triple = "x86_64-unknown-linux-gnu"`, "define i32 @main()", "declare void @apus_trap(i32) noreturn", "call void @apus_flush()", "ret i32"} {
		if !strings.Contains(m, want) {
			t.Errorf("module missing %q", want)
		}
	}
	if strings.Contains(emit(t, `fn main() {}`, ""), "target triple") {
		t.Error("no triple must be emitted when none is requested")
	}
}

func TestCheckedArithmeticLowering(t *testing.T) {
	m := emit(t, `fn main() io { let a = io::read_u64(); let b = io::read_u64(); io::println(str(a + b)); let c = io::read_i64(); io::println(str(c * c)); }`, "")
	for _, want := range []string{"llvm.uadd.with.overflow.i64", "llvm.smul.with.overflow.i64", "call void @apus_trap(i32 1)"} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %q", want)
		}
	}
	m = emit(t, `fn main() io { let a = io::read_u32(); let b = io::read_u32(); io::println(str(a / b)); }`, "")
	if !strings.Contains(m, "call void @apus_trap(i32 2)") || !strings.Contains(m, "udiv i64") {
		t.Error("division must check for zero and use udiv")
	}
}

func TestUncheckedArithmeticHasNoTraps(t *testing.T) {
	m := emit(t, `fn main() io { let a = clamp(io::read_u32(), 0, 10); let b = clamp(io::read_u32(), 0, 10); io::println(str(a + b)); }`, "")
	if strings.Contains(m, "call void @apus_trap(i32 1)") {
		t.Error("proven-safe addition should not contain an overflow trap")
	}
}

func TestStringConstantEscaping(t *testing.T) {
	m := emit(t, `fn main() io { io::println("a\"b\\c\n\t"); }`, "")
	if !strings.Contains(m, `c"a\22b\5Cc\0A\09\0A"`) {
		t.Errorf("escaping wrong:\n%s", m)
	}
}

func TestLoopBoundTrap(t *testing.T) {
	m := emit(t, `fn main() io { let n = io::read_u32(); let mut i: u32 = 0; while i < n bound(9) { i += 1; } }`, "")
	if !strings.Contains(m, "call void @apus_trap(i32 4)") || !strings.Contains(m, "icmp uge i64") {
		t.Error("bounded dynamic loop must trap when the bound is exceeded")
	}
}

func TestRuntimeEmbedded(t *testing.T) {
	for _, fn := range []string{"apus_write", "apus_write_dec", "apus_read_line", "apus_read_int", "apus_trap", "apus_flush"} {
		if !strings.Contains(llvm.Runtime, fn) {
			t.Errorf("runtime lacks %s", fn)
		}
	}
}

// If clang is installed, the module must be valid LLVM and link against the runtime.
func TestClangAcceptsModule(t *testing.T) {
	clang, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("clang not installed")
	}
	dir := t.TempDir()
	m := emit(t, `struct P { a: u32, b: [u8; 3] }
	fn main() io { let n = clamp(io::read_u32(), 0, 5); let mut a = [1, 2, 3]; for i in 0..n { a[i as u64 % 3] += 1; } io::println(str(a[0])); }`, "")
	ll := filepath.Join(dir, "m.ll")
	rt := filepath.Join(dir, "rt.c")
	os.WriteFile(ll, []byte(m), 0o644)
	os.WriteFile(rt, []byte(llvm.Runtime), 0o644)
	cmd := exec.Command(clang, "-O2", "-Wall", "-Wno-override-module", "-o", filepath.Join(dir, "a.out"), ll, rt)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clang rejected the module: %v\n%s\n%s", err, out, m)
	}
}

func TestRodataTableLowering(t *testing.T) {
	m := emit(t, `const T: [u8; 4] = [1, 2, 3, 250]; fn main() io { let i = io::read_u32(); io::println(str(T[(i & 3) as u64])); }`, "")
	if !strings.Contains(m, `@tab0 = private unnamed_addr constant [4 x i8] c"\01\02\03\FA"`) {
		t.Errorf("table must be a private constant global:\n%s", m)
	}
	if strings.Contains(m, "alloca [4 x") {
		t.Error("a constant table must not be copied to the stack")
	}
	w := emit(t, `const T: [u32; 3] = [4000000000, 2, 3]; fn main() io { let i = clamp(io::read_u32(), 0, 2); io::println(str(T[i as u64])); }`, "")
	if !strings.Contains(w, "[3 x i32] [i32 -294967296, i32 2, i32 3]") {
		t.Errorf("wide tables keep their element width:\n%s", w)
	}
}

func TestStringEqualityLowering(t *testing.T) {
	m := emit(t, `fn main() io { let s = io::read_line(8); if s == "quit" { io::println("bye"); } }`, "")
	if !strings.Contains(m, "call i32 @apus_memeq") || !strings.Contains(m, "icmp eq i64") {
		t.Error("runtime string == literal must compare length then bytes")
	}
}

func TestClangAcceptsNewFeatures(t *testing.T) {
	clang, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("clang not installed")
	}
	dir := t.TempDir()
	m := emit(t, `enum Op { Add, Mul }
	const T: [u16; 4] = [1, 20, 300, 4000];
	fn main() io {
	    let s = io::read_line(8);
	    let n = clamp(io::read_u32(), 0, 5);
	    let mut acc: u32 = 0;
	    for i in 0..n {
	        if i == 3 { continue; }
	        if s == "stop" { break; }
	        match i { 0 => acc += 1, 1 | 2 => acc += T[i as u64] as u32, _ => acc += s[0] as u32 }
	    }
	    io::println(str(acc));
	}`, "")
	ll, rt := filepath.Join(dir, "m.ll"), filepath.Join(dir, "rt.c")
	os.WriteFile(ll, []byte(m), 0o644)
	os.WriteFile(rt, []byte(llvm.Runtime), 0o644)
	if out, err := exec.Command(clang, "-O2", "-Wall", "-Wno-override-module", "-o", filepath.Join(dir, "a.out"), ll, rt).CombinedOutput(); err != nil {
		t.Fatalf("clang rejected the module: %v\n%s", err, out)
	}
}

func TestFloatLowering(t *testing.T) {
	m := emit(t, `fn main() io {
	    let x = io::read_f64();
	    let y = io::read_f32();
	    io::println(str(sqrt(x) * 2.0 + floor(x)) ++ str(y + 1.0) ++ str(x as i32) ++ str(min(x, 1.0)));
	}`, "")
	for _, want := range []string{"fmul double", "fadd double", "llvm.sqrt.f64(", "fadd float", "call i64 @apus_read_f64(i32 1)",
		"@apus_fmt_f64", "fcmp olt double", "fptosi double", "call void @apus_trap(i32 1)", "llvm.trunc.f64"} {
		if !strings.Contains(m, want) {
			t.Errorf("module missing %q", want)
		}
	}
}

// Fast-math or contraction flags would let the optimizer change results and break
// the bit-for-bit agreement between folded, interpreted and native floating point.
func TestNoFastMathFlags(t *testing.T) {
	m := emit(t, `fn main() io { let a = io::read_f64(); let b = a * 3.0 + 1.0; let c = b / a - sqrt(b); io::println(str(c)); }`, "")
	for _, flag := range []string{" fast ", " contract ", " reassoc ", " nnan ", " ninf ", " afn "} {
		if strings.Contains(m, flag) {
			t.Errorf("emitted IR must not use the %q flag", flag)
		}
	}
}

func TestFloatCompareSemantics(t *testing.T) {
	m := emit(t, `fn main() io { let a = io::read_f64(); let b = io::read_f64();
	    if a == b { io::println("eq"); } if a != b { io::println("ne"); } if a < b { io::println("lt"); } if a >= b { io::println("ge"); } if is_nan(a) { io::println("nan"); } }`, "")
	for _, want := range []string{"fcmp oeq", "fcmp une", "fcmp olt", "fcmp oge", "fcmp uno"} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %q: != must be unordered, the rest ordered", want)
		}
	}
}

func TestFloatConstantTable(t *testing.T) {
	m := emit(t, `const T: [f32; 3] = [0.5, 1.0, 2.0]; fn main() io { let i = clamp(io::read_u32(), 0, 2); io::println(str(T[i as u64])); }`, "")
	if !strings.Contains(m, "[3 x i32] [i32 1056964608, i32 1065353216, i32 1073741824]") {
		t.Errorf("f32 table should hold IEEE bit patterns:\n%s", m)
	}
}

func TestRuntimeProvidesRounding(t *testing.T) {
	for _, fn := range []string{"floor(", "ceil(", "trunc(", "apus_fmt_f64", "apus_read_f64", "apus_write_f64"} {
		if !strings.Contains(llvm.Runtime, fn) {
			t.Errorf("runtime lacks %s", fn)
		}
	}
}
