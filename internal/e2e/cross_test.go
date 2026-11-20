package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"apus/internal/driver"
	"apus/internal/ir"
	"apus/internal/llvm"
	"apus/internal/peval"
)

type crossTarget struct {
	name, triple, qemu string
	lld                bool
}

func buildCross(t *testing.T, prog *ir.Program, name string, ct crossTarget) string {
	t.Helper()
	dir := filepath.Join(tmpDir, "cross-"+ct.name)
	os.MkdirAll(dir, 0o755)
	ll, rt, exe := filepath.Join(dir, name+".ll"), filepath.Join(dir, "rt.c"), filepath.Join(dir, name+".bin")
	os.WriteFile(ll, []byte(llvm.Emit(prog, "")), 0o644)
	os.WriteFile(rt, []byte(llvm.Runtime), 0o644)
	args := llvm.ClangArgs(llvm.BuildOptions{Target: ct.triple, OptLevel: "z", Freestanding: true, LLD: ct.lld, Out: exe})
	args = append(args, ll, rt)
	if out, err := exec.Command(clangPath, args...).CombinedOutput(); err != nil {
		t.Fatalf("clang failed for %s: %v\n%s", ct.name, err, out)
	}
	return exe
}

// Runs the whole program corpus with the libc-free runtime: natively on the
// host, and on AArch64 / RISC-V under QEMU user-mode emulation when available.
func TestFreestandingAndCrossTargets(t *testing.T) {
	if clangPath == "" {
		t.Skip("clang not available")
	}
	targets := []crossTarget{{name: "host", triple: ""}}
	if _, err := exec.LookPath("ld.lld"); err == nil {
		for _, c := range []crossTarget{
			{"aarch64", "aarch64-linux-gnu", "qemu-aarch64", true},
			{"riscv64", "riscv64-linux-gnu", "qemu-riscv64", true},
		} {
			if _, err := exec.LookPath(c.qemu); err == nil {
				targets = append(targets, c)
			}
		}
	}
	files, _ := filepath.Glob("../../testdata/programs/*.apus")
	sort.Strings(files)
	for _, ct := range targets {
		ct := ct
		t.Run(ct.name, func(t *testing.T) {
			for _, f := range files {
				name := strings.TrimSuffix(filepath.Base(f), ".apus")
				srcb, _ := os.ReadFile(f)
				sp := parseSpec(t, string(srcb))
				out, derr := driver.CompileIn(string(srcb), filepath.Dir(f), peval.DefaultOptions())
				if derr != nil {
					t.Fatal(derr)
				}
				exe := buildCross(t, out.Res.Prog, name, ct)
				for i, rc := range sp.runs {
					want := runVM(out.Res.Prog, rc.stdin)
					var cmd *exec.Cmd
					if ct.qemu != "" {
						cmd = exec.Command(ct.qemu, exe)
					} else {
						cmd = exec.Command(exe)
					}
					cmd.Stdin = strings.NewReader(rc.stdin)
					var so, se bytes.Buffer
					cmd.Stdout, cmd.Stderr = &so, &se
					err := cmd.Run()
					code := 0
					if ee, ok := err.(*exec.ExitError); ok {
						code = ee.ExitCode()
					}
					if so.String() != want.stdout || code != want.exit {
						t.Errorf("%s run%d on %s: got out=%q exit=%d (stderr %q), want out=%q exit=%d",
							name, i, ct.name, so.String(), code, se.String(), want.stdout, want.exit)
					}
				}
			}
			fmt.Printf("freestanding corpus ok on %s\n", ct.name)
		})
	}
}
