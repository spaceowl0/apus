// Command apus is the Apus compiler driver.
package main

import (
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"apus/internal/diag"
	"apus/internal/driver"
	"apus/internal/ir"
	"apus/internal/llvm"
	"apus/internal/peval"
)

const version = "0.8.0"

func usage() {
	fmt.Fprintf(os.Stderr, `apus %s - compiler for the Apus language

usage: apus <command> [flags] file.apus

commands:
  check    parse and type-check only
  ir       print the residual IR after partial evaluation
  run      compile and execute in the built-in VM (stdin/stdout passthrough)
  llvm     emit textual LLVM IR            (flags: -target <triple>)
  build    produce a native executable via clang
           (flags: -o <out> -target <triple> -O <0|1|2|3|s|z> -clang <path>
                   -freestanding  no libc: raw syscalls + custom _start (Linux x86_64/aarch64)
                   -lld           link with lld (needed to cross-link, e.g. aarch64 from x86_64))
  report   print partial-evaluation and memory-budget statistics

common flags:
  -fuel N     compile-time evaluation step limit (default %d)
  -unroll N   maximum fully-unrolled trip count (default %d)
  -D NAME=V   override a typed integer/bool const at build time (repeatable)
`, version, peval.DefaultOptions().Fuel, peval.DefaultOptions().UnrollLimit)
}

func main() {
	if len(os.Args) < 3 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	opt := peval.DefaultOptions()
	fs.IntVar(&opt.Fuel, "fuel", opt.Fuel, "")
	fs.IntVar(&opt.UnrollLimit, "unroll", opt.UnrollLimit, "")
	target := fs.String("target", "", "")
	outPath := fs.String("o", "a.out", "")
	olevel := fs.String("O", "2", "")
	clang := fs.String("clang", "clang", "")
	var defs defineFlags
	fs.Var(&defs, "D", "")
	freestanding := fs.Bool("freestanding", false, "")
	lld := fs.Bool("lld", false, "")
	fs.Usage = usage
	fs.Parse(os.Args[2:])
	if fs.NArg() != 1 {
		usage()
		os.Exit(2)
	}
	path := fs.Arg(0)
	srcb, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	src := string(srcb)
	out, derr := driver.CompileWith(src, driver.Options{BaseDir: filepath.Dir(path), Defines: defs.m, PE: opt})
	if derr != nil {
		fmt.Fprint(os.Stderr, diag.Render(derr, path, src))
		os.Exit(1)
	}
	for _, n := range out.Res.Notes {
		fmt.Fprintf(os.Stderr, "note: %s:%s\n", path, n)
	}
	for _, w := range out.Res.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s:%s\n", path, w)
	}
	switch cmd {
	case "check":
		fmt.Println("ok")
	case "ir":
		fmt.Print(ir.Format(out.Res.Prog))
	case "run":
		r := ir.Run(out.Res.Prog, os.Stdin, os.Stdout)
		if r.Trap != ir.TrapNone {
			fmt.Fprintf(os.Stderr, "apus: runtime trap: %s\n", r.Trap)
		}
		os.Exit(r.ExitCode(out.Res.Prog))
	case "llvm":
		fmt.Print(llvm.Emit(out.Res.Prog, *target))
	case "build":
		if err := build(out, *outPath, *target, *olevel, *clang, *freestanding, *lld); err != nil {
			fmt.Fprintln(os.Stderr, "apus build:", err)
			os.Exit(1)
		}
	case "report":
		report(out)
	default:
		usage()
		os.Exit(2)
	}
}

func build(out *driver.Output, outPath, target, olevel, clang string, freestanding, lld bool) error {
	dir, err := os.MkdirTemp("", "apus-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ll := filepath.Join(dir, "prog.ll")
	rt := filepath.Join(dir, "rt.c")
	if err := os.WriteFile(ll, []byte(llvm.Emit(out.Res.Prog, "")), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(rt, []byte(llvm.Runtime), 0o644); err != nil {
		return err
	}
	args := llvm.ClangArgs(llvm.BuildOptions{Target: target, OptLevel: olevel, Freestanding: freestanding, LLD: lld, Out: outPath})
	args = append(args, ll, rt)
	cmd := exec.Command(clang, args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func report(out *driver.Output) {
	s := out.Res.Stats
	fmt.Printf("residual instructions : %d (before DCE: %d)\n", s.ResidualInsts, s.InstsBeforeOpt)
	fmt.Printf("frame size            : %d bytes\n", out.Res.Prog.FrameBytes())
	fmt.Printf("fuel used             : %d\n", s.FuelUsed)
	fmt.Printf("folded ops            : %d\n", s.FoldedOps)
	fmt.Printf("branches removed      : %d\n", s.BranchesRemoved)
	fmt.Printf("pure calls folded     : %d (memo hits %d)\n", s.PureCallsFolded, s.MemoHits)
	fmt.Printf("unrolled iterations   : %d\n", s.UnrolledIters)
	fmt.Printf("residual loops        : %d\n", s.DynLoops)
	for _, a := range out.Res.Arenas {
		fmt.Printf("arena %-12s size %6d  high-water %6d (%.1f%%)\n", a.Name, a.Size, a.HighWater, 100*float64(a.HighWater)/float64(a.Size))
	}
}

// defineFlags collects repeated -D NAME=VALUE options.
type defineFlags struct{ m map[string]*big.Int }

func (d *defineFlags) String() string { return "" }

func (d *defineFlags) Set(s string) error {
	name, val, ok := strings.Cut(s, "=")
	if !ok || name == "" {
		return fmt.Errorf("-D expects NAME=VALUE, got %q", s)
	}
	if d.m == nil {
		d.m = map[string]*big.Int{}
	}
	switch val {
	case "true":
		d.m[name] = big.NewInt(1)
	case "false":
		d.m[name] = big.NewInt(0)
	default:
		v, ok := new(big.Int).SetString(strings.ReplaceAll(val, "_", ""), 0)
		if !ok {
			return fmt.Errorf("-D %s: %q is not an integer or bool", name, val)
		}
		d.m[name] = v
	}
	return nil
}
