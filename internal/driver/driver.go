// Package driver wires the compiler phases together.
package driver

import (
	"math/big"

	"apus/internal/diag"
	"apus/internal/ir"
	"apus/internal/peval"
	"apus/internal/sema"
	"apus/internal/syntax"
)

type Output struct {
	Info *sema.Info
	Res  *peval.Result
}

// Options configure a compilation.
type Options struct {
	BaseDir string              // root for embed("...") paths
	Defines map[string]*big.Int // -D overrides for typed int/bool constants
	PE      peval.Options
}

// Compile parses, checks and partially evaluates src (embed() is unavailable).
func Compile(src string, opt peval.Options) (*Output, *diag.Error) {
	return CompileWith(src, Options{PE: opt})
}

// CompileIn is Compile with baseDir as the root for embed("...") paths.
func CompileIn(src, baseDir string, opt peval.Options) (*Output, *diag.Error) {
	return CompileWith(src, Options{BaseDir: baseDir, PE: opt})
}

// CompileWith runs the whole front end and the partial evaluator.
func CompileWith(src string, o Options) (*Output, *diag.Error) {
	var prog *syntax.Program
	if err := diag.Try(func() { prog = syntax.Parse(src) }); err != nil {
		return nil, err
	}
	var info *sema.Info
	if err := diag.Try(func() { info = sema.CheckOpts(prog, sema.Options{BaseDir: o.BaseDir, Defines: o.Defines}) }); err != nil {
		return nil, err
	}
	o.PE.Defines = o.Defines
	res, err := peval.Compile(info, o.PE)
	if err != nil {
		return nil, err
	}
	return &Output{Info: info, Res: res}, nil
}

// Program is a convenience returning only the IR program.
func Program(src string) (*ir.Program, *diag.Error) {
	o, err := Compile(src, peval.DefaultOptions())
	if err != nil {
		return nil, err
	}
	return o.Res.Prog, nil
}
