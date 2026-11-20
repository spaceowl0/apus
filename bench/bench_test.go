// Package bench contains the Apus benchmark suites.
//
//	go test ./bench -bench . -benchmem
//
// Compile benchmarks measure the cost of aggressive partial evaluation.
// Run benchmarks compare three ways of executing the same program:
//   - ref: a tree-walking interpreter (no partial evaluation)
//   - vm : the residual IR produced by the compiler, in the IR VM
//
// Native benchmarks live in bench/native (see run.sh).
package bench

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"apus/internal/driver"
	"apus/internal/ir"
	"apus/internal/peval"
	"apus/internal/ref"
)

func load(b *testing.B, name string) string {
	b.Helper()
	return loadFile(b, name)
}

func loadFile(b *testing.B, name string) string {
	b.Helper()
	data, err := os.ReadFile("../testdata/programs/" + name + ".apus")
	if err != nil {
		b.Fatal(err)
	}
	return string(data)
}

func BenchmarkCompile(b *testing.B) {
	for _, name := range []string{
		"p01_hello", "p02_fib_static", "p03_fib_dynamic", "p15_sieve_static", "p16_matmul",
		"p17_bubble", "p24_structarr", "p13_arena", "p22_ackermann",
		"p40_crc_table", "p41_const_parse", "p31_enum_match", "p33_break_continue",
	} {
		src := load(b, name)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := driver.Compile(src, peval.DefaultOptions()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Compile time as a function of how much static work the evaluator performs.
func BenchmarkCompileScaling(b *testing.B) {
	for _, n := range []int{10, 25, 50, 75, 90} {
		src := fmt.Sprintf(`pure fn fib(n: u32) -> u64 bound(rec = 93) { if n < 2 { return n as u64; } return fib(n - 1) + fib(n - 2); }
fn main() -> u64 { return static fib(%d); }`, n)
		b.Run(fmt.Sprintf("fib%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := driver.Compile(src, peval.DefaultOptions()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	for _, n := range []int{16, 64, 256} {
		src := fmt.Sprintf(`fn main() io { let mut s: u64 = 0; for i in 0..%d { s += (i * i) as u64; } io::println(str(s)); }`, n)
		b.Run(fmt.Sprintf("unroll%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := driver.Compile(src, peval.DefaultOptions()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type workload struct {
	name, src, stdin string
}

var workloads = []workload{
	{"fib_recursive_25", `pure fn fib(n: u32) -> u64 bound(rec = 93) { if n < 2 { return n as u64; } return fib(n - 1) + fib(n - 2); }
fn main() io { io::println(str(fib(25))); }`, ""},
	{"sieve_primes_below_100", "", ""},
	{"collatz_dynamic", "", "27\n"},
	{"gcd_dynamic", "", "1071 462\n"},
	{"dyn_loop_sum_1000", "", ""},
}

func init() {
	data, _ := os.ReadFile("../testdata/programs/p15_sieve_static.apus")
	workloads[1].src = string(data)
	data, _ = os.ReadFile("../testdata/programs/p05_collatz.apus")
	workloads[2].src = string(data)
	data, _ = os.ReadFile("../testdata/programs/p14_gcd.apus")
	workloads[3].src = string(data)
	data, _ = os.ReadFile("../testdata/programs/p23_bigloop.apus")
	workloads[4].src = string(data)
}

func BenchmarkRun(b *testing.B) {
	for _, w := range workloads {
		out, err := driver.Compile(w.src, peval.DefaultOptions())
		if err != nil {
			b.Fatalf("%s: %v", w.name, err)
		}
		b.Run(w.name+"/vm", func(b *testing.B) {
			var steps int64
			for i := 0; i < b.N; i++ {
				r := ir.Run(out.Res.Prog, strings.NewReader(w.stdin), io.Discard)
				steps = r.Steps
			}
			b.ReportMetric(float64(steps), "insts/op")
		})
		b.Run(w.name+"/ref", func(b *testing.B) {
			var calls int
			for i := 0; i < b.N; i++ {
				var buf bytes.Buffer
				r := ref.Run(out.Info, strings.NewReader(w.stdin), &buf)
				calls = r.Calls
			}
			b.ReportMetric(float64(calls), "calls/op")
		})
	}
}

// Residual size: how many instructions survive partial evaluation.
func TestResidualSizeReport(t *testing.T) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-22s %8s %8s %8s %8s\n", "program", "before", "after", "frameB", "fuel")
	for _, name := range []string{"p01_hello", "p02_fib_static", "p06_sumsq", "p15_sieve_static", "p16_matmul", "p22_ackermann", "p17_bubble", "p13_arena"} {
		data, err := os.ReadFile("../testdata/programs/" + name + ".apus")
		if err != nil {
			t.Fatal(err)
		}
		o, derr := driver.Compile(string(data), peval.DefaultOptions())
		if derr != nil {
			t.Fatal(derr)
		}
		s := o.Res.Stats
		fmt.Fprintf(&sb, "%-22s %8d %8d %8d %8d\n", name, s.InstsBeforeOpt, s.ResidualInsts, o.Res.Prog.FrameBytes(), s.FuelUsed)
	}
	t.Log("\n" + sb.String())
}
