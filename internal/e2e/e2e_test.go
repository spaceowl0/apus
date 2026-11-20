// Package e2e runs every program in testdata/programs through all backends
// (residual-IR VM, reference interpreter and, when available, native code built
// with clang) and checks them against the expectations written in the file.
package e2e

import (
	"bytes"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"apus/internal/driver"
	"apus/internal/ir"
	"apus/internal/llvm"
	"apus/internal/peval"
	"apus/internal/ref"
)

type runCase struct {
	stdin, stdout string
	exit          int
	hasExit       bool
	trap          string
}

type spec struct {
	runs     []runCase
	maxInsts int
	arenas   map[string]int
}

var (
	clangPath string
	rtObj     string
	tmpDir    string
)

func TestMain(m *testing.M) {
	tmpDir, _ = os.MkdirTemp("", "apus-e2e-")
	if p, err := exec.LookPath("clang"); err == nil && os.Getenv("APUS_NO_NATIVE") == "" {
		rt := filepath.Join(tmpDir, "rt.c")
		os.WriteFile(rt, []byte(llvm.Runtime), 0o644)
		obj := filepath.Join(tmpDir, "rt.o")
		if err := exec.Command(p, "-O2", "-c", "-o", obj, rt).Run(); err == nil {
			clangPath, rtObj = p, obj
		}
	}
	code := m.Run()
	os.RemoveAll(tmpDir)
	os.Exit(code)
}

func readQuoted(s string) (string, string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, `"`) {
		return "", "", fmt.Errorf("expected quoted string in %q", s)
	}
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '"' {
			u, err := strconv.Unquote(s[:i+1])
			return u, s[i+1:], err
		}
	}
	return "", "", fmt.Errorf("unterminated string in %q", s)
}

func parseSpec(t *testing.T, src string) spec {
	sp := spec{maxInsts: -1, arenas: map[string]int{}}
	for _, ln := range strings.Split(src, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(ln, "// run:"):
			rest := strings.TrimPrefix(ln, "// run:")
			in, rest, err := readQuoted(rest)
			if err != nil {
				t.Fatal(err)
			}
			rest = strings.TrimSpace(rest)
			if !strings.HasPrefix(rest, "=>") {
				t.Fatalf("missing => in %q", ln)
			}
			out, rest, err := readQuoted(strings.TrimPrefix(rest, "=>"))
			if err != nil {
				t.Fatal(err)
			}
			rc := runCase{stdin: in, stdout: out}
			rest = strings.TrimSpace(rest)
			if strings.HasPrefix(rest, "exit=") {
				f := strings.Fields(rest)[0]
				rc.exit, _ = strconv.Atoi(strings.TrimPrefix(f, "exit="))
				rc.hasExit = true
				rest = strings.TrimSpace(strings.TrimPrefix(rest, f))
			}
			if strings.HasPrefix(rest, "trap=") {
				rc.trap = strings.TrimPrefix(rest, "trap=")
			}
			sp.runs = append(sp.runs, rc)
		case strings.HasPrefix(ln, "// insts<="):
			sp.maxInsts, _ = strconv.Atoi(strings.TrimPrefix(ln, "// insts<="))
		case strings.HasPrefix(ln, "// arena:"):
			f := strings.Fields(strings.TrimPrefix(ln, "// arena:"))
			n, _ := strconv.Atoi(f[1])
			sp.arenas[f[0]] = n
		}
	}
	return sp
}

type outcome struct {
	stdout string
	trap   ir.TrapCode
	exit   int
}

func runVM(p *ir.Program, stdin string) outcome {
	var out bytes.Buffer
	r := ir.Run(p, strings.NewReader(stdin), &out)
	return outcome{out.String(), r.Trap, r.ExitCode(p)}
}

func buildNative(t *testing.T, p *ir.Program, name string) string {
	t.Helper()
	ll := filepath.Join(tmpDir, name+".ll")
	exe := filepath.Join(tmpDir, name+".bin")
	if err := os.WriteFile(ll, []byte(llvm.Emit(p, "")), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(clangPath, "-O1", "-Wno-override-module", "-o", exe, ll, rtObj)
	if outb, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clang failed: %v\n%s", err, outb)
	}
	return exe
}

func runNative(exe, stdin string) outcome {
	cmd := exec.Command(exe)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	oc := outcome{stdout: out.String(), exit: code}
	if code == 101 && strings.Contains(errb.String(), "runtime trap") {
		for c := ir.TrapOverflow; c <= ir.TrapInput; c++ {
			if strings.Contains(errb.String(), c.String()) {
				oc.trap = c
			}
		}
	}
	return oc
}

func TestPrograms(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/programs/*.apus")
	if len(files) < 20 {
		t.Fatalf("expected the program corpus, found %d files", len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".apus")
		t.Run(name, func(t *testing.T) {
			srcb, _ := os.ReadFile(f)
			src := string(srcb)
			sp := parseSpec(t, src)
			out, derr := driver.CompileIn(src, filepath.Dir(f), peval.DefaultOptions())
			if derr != nil {
				t.Fatalf("compile error: %v", derr)
			}
			prog := out.Res.Prog
			if sp.maxInsts >= 0 && out.Res.Stats.ResidualInsts > sp.maxInsts {
				t.Errorf("residual instructions = %d, want <= %d\n%s", out.Res.Stats.ResidualInsts, sp.maxInsts, ir.Format(prog))
			}
			for _, a := range out.Res.Arenas {
				if want, ok := sp.arenas[a.Name]; ok && a.HighWater != want {
					t.Errorf("arena %s high-water = %d, want %d", a.Name, a.HighWater, want)
				}
			}
			var exe string
			if clangPath != "" {
				exe = buildNative(t, prog, name)
			}
			for i, rc := range sp.runs {
				rc := rc
				t.Run(fmt.Sprintf("run%d", i), func(t *testing.T) {
					vm := runVM(prog, rc.stdin)
					if vm.stdout != rc.stdout {
						t.Errorf("vm stdout = %q, want %q", vm.stdout, rc.stdout)
					}
					wantTrap := ir.TrapNone
					for c := ir.TrapOverflow; c <= ir.TrapInput; c++ {
						if c.String() == rc.trap {
							wantTrap = c
						}
					}
					if rc.trap != "" && wantTrap == ir.TrapNone {
						t.Fatalf("unknown trap name %q in spec", rc.trap)
					}
					if vm.trap != wantTrap {
						t.Errorf("vm trap = %v, want %v", vm.trap, wantTrap)
					}
					if rc.hasExit && vm.exit != rc.exit {
						t.Errorf("vm exit = %d, want %d", vm.exit, rc.exit)
					}
					// reference interpreter
					var rout bytes.Buffer
					rr := ref.RunLimited(out.Info, strings.NewReader(rc.stdin), &rout, 2_000_000)
					if rr.Exceeded {
						t.Logf("reference interpreter exceeded its call budget (expected for programs that rely on partial evaluation)")
					} else if rout.String() != vm.stdout || rr.Trap != vm.trap {
						t.Errorf("ref disagrees with vm: ref out=%q trap=%v, vm out=%q trap=%v", rout.String(), rr.Trap, vm.stdout, vm.trap)
					}
					if !rr.Exceeded && rr.Trap == ir.TrapNone && rr.Ret != nil {
						low := int(new(big.Int).And(rr.Ret, big.NewInt(0xff)).Int64())
						if low != vm.exit {
							t.Errorf("ref exit %d != vm exit %d", low, vm.exit)
						}
					}
					if exe != "" {
						nat := runNative(exe, rc.stdin)
						if nat.stdout != vm.stdout || nat.exit != vm.exit || nat.trap != vm.trap {
							t.Errorf("native disagrees with vm: native out=%q exit=%d trap=%v; vm out=%q exit=%d trap=%v",
								nat.stdout, nat.exit, nat.trap, vm.stdout, vm.exit, vm.trap)
						}
					}
				})
			}
		})
	}
}
