// Package sema type-checks Apus programs and annotates the AST with types.
package sema

import (
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"apus/internal/diag"
	"apus/internal/syntax"
	"apus/internal/types"
)

// Options configure checking. BaseDir is the directory embed("...") paths are
// resolved against (and may not escape); empty disables embed.
type Options struct {
	BaseDir string
	// Defines overrides the value of typed integer/bool constants (apus -D NAME=VALUE).
	Defines map[string]*big.Int
}

type Info struct {
	Enums   map[string]*types.Type
	Structs map[string]*types.Type
	Consts  map[string]*syntax.ConstDecl
	Arenas  map[string]*syntax.ArenaDecl
	Fns     map[string]*syntax.FnDecl
	Prog    *syntax.Program
}

type local struct {
	ty  *types.Type
	mut bool
}

type checker struct {
	typeSubst  map[string]*types.Type // active generic type arguments
	constSubst map[string]*big.Int    // active generic const arguments
	constTy    map[string]*types.Type
	opts       Options
	loopDepth  int
	info       *Info
	scopes     []map[string]*local
	fn         *syntax.FnDecl
	visit      map[string]bool // const cycle detection
	constOK    map[string]bool
}

func errf(code string, pos diag.Pos, f string, a ...any) {
	diag.Fail(diag.Errf(code, pos, f, a...))
}

func errh(code string, pos diag.Pos, help string, f string, a ...any) {
	diag.Fail(diag.Errf(code, pos, f, a...).WithHelp(help))
}

// Check type-checks prog. It panics with *diag.Error on failure.
func Check(prog *syntax.Program) *Info { return CheckOpts(prog, Options{}) }

// CheckOpts is Check with options.
func CheckOpts(prog *syntax.Program, opts Options) *Info {
	info := &Info{
		Enums:   map[string]*types.Type{},
		Structs: map[string]*types.Type{}, Consts: map[string]*syntax.ConstDecl{},
		Arenas: map[string]*syntax.ArenaDecl{}, Fns: map[string]*syntax.FnDecl{}, Prog: prog,
	}
	c := &checker{opts: opts, info: info, visit: map[string]bool{}, constOK: map[string]bool{}}
	for _, ed := range prog.Enums {
		if info.Enums[ed.Name] != nil || info.Structs[ed.Name] != nil {
			errf("E0004", ed.Pos, "duplicate type `%s`", ed.Name)
		}
		t := &types.Type{Kind: types.KEnum, Name: ed.Name, Bits: 32}
		next := int64(0)
		seenName, seenVal := map[string]bool{}, map[int64]bool{}
		for _, v := range ed.Variants {
			if v.Val != nil {
				if !v.Val.IsInt64() || v.Val.Sign() < 0 || v.Val.Int64() > 1<<32-1 {
					errf("E0301", ed.Pos, "enum value %s does not fit in u32", v.Val)
				}
				next = v.Val.Int64()
			}
			if seenName[v.Name] || seenVal[next] {
				errf("E0004", ed.Pos, "duplicate variant name or value in enum `%s`", ed.Name)
			}
			seenName[v.Name], seenVal[next] = true, true
			t.Variants = append(t.Variants, types.Variant{Name: v.Name, Value: next})
			next++
		}
		info.Enums[ed.Name] = t
	}
	for _, s := range prog.Structs {
		if info.Structs[s.Name] != nil || info.Enums[s.Name] != nil {
			errf("E0004", s.Pos, "duplicate struct `%s`", s.Name)
		}
		t := &types.Type{Kind: types.KStruct, Name: s.Name}
		seen := map[string]bool{}
		info.Structs[s.Name] = t // allows no recursion since fields resolve before use below
		for _, f := range s.Fields {
			if seen[f.Name] {
				errf("E0004", f.TE.Pos, "duplicate field `%s`", f.Name)
			}
			seen[f.Name] = true
			ft := c.resolve(f.TE, false)
			if ft.Kind == types.KStruct && ft.Name == s.Name {
				errf("E0004", f.TE.Pos, "recursive struct `%s` has infinite size", s.Name)
			}
			t.Fields = append(t.Fields, types.Field{Name: f.Name, Ty: ft})
		}
	}
	for _, a := range prog.Arenas {
		if info.Arenas[a.Name] != nil {
			errf("E0004", a.Pos, "duplicate arena `%s`", a.Name)
		}
		info.Arenas[a.Name] = a
	}
	for _, cd := range prog.Consts {
		if info.Consts[cd.Name] != nil {
			errf("E0004", cd.Pos, "duplicate const `%s`", cd.Name)
		}
		info.Consts[cd.Name] = cd
	}
	for _, f := range prog.Fns {
		if info.Fns[f.Name] != nil {
			errf("E0004", f.Pos, "duplicate function `%s`", f.Name)
		}
		info.Fns[f.Name] = f
		if len(f.Generics) > 0 {
			if f.Name == "main" {
				errf("E0003", f.Pos, "`main` cannot be generic")
			}
			continue // checked per instantiation
		}
		for i := range f.Params {
			f.Params[i].Ty = c.resolve(f.Params[i].TE, false)
			p := f.Params[i]
			if p.Lo != nil && !p.Ty.IsInt() {
				errf("E0305", p.TE.Pos, "range refinement requires an integer parameter")
			}
		}
		f.Ret = types.Unit
		if f.RetTE != nil {
			f.Ret = c.resolve(f.RetTE, false)
		}
	}
	c.push()
	for _, cd := range prog.Consts {
		c.checkConst(cd)
	}
	c.pop()
	var defNames []string
	for n := range opts.Defines {
		defNames = append(defNames, n)
	}
	sort.Strings(defNames)
	for _, n := range defNames {
		cd := info.Consts[n]
		if cd == nil {
			errf("E0312", diag.Pos{Line: 1, Col: 1}, "-D %s: no such constant", n)
		}
		if cd.Ty.Kind != types.KInt && cd.Ty.Kind != types.KBool {
			errf("E0312", cd.Pos, "-D %s: only integer and bool constants can be overridden", n)
		}
		lo, hi := cd.Ty.Range()
		if v := opts.Defines[n]; v.Cmp(lo) < 0 || v.Cmp(hi) > 0 {
			errf("E0312", cd.Pos, "-D %s=%s does not fit in `%s`", n, v, cd.Ty)
		}
	}
	for _, f := range append([]*syntax.FnDecl{}, prog.Fns...) {
		if len(f.Generics) == 0 && f.Ret != nil {
			c.checkFn(f)
		}
	}
	m := info.Fns["main"]
	if m == nil {
		errf("E0003", diag.Pos{Line: 1, Col: 1}, "no `main` function")
	}
	if len(m.Params) != 0 {
		errf("E0003", m.Pos, "`main` must take no parameters")
	}
	if m.Ret.Kind != types.KUnit && m.Ret.Kind != types.KInt {
		errf("E0003", m.Pos, "`main` must return () or an integer type")
	}
	c.checkRecursion()
	return info
}

func (c *checker) push() { c.scopes = append(c.scopes, map[string]*local{}) }
func (c *checker) pop()  { c.scopes = c.scopes[:len(c.scopes)-1] }
func (c *checker) declare(name string, t *types.Type, mut bool) {
	c.scopes[len(c.scopes)-1][name] = &local{t, mut}
}
func (c *checker) lookup(name string) *local {
	for i := len(c.scopes) - 1; i >= 0; i-- {
		if l, ok := c.scopes[i][name]; ok {
			return l
		}
	}
	return nil
}

// resolve converts a syntactic type to a semantic type. When allowStatic is true,
// `StaticStr` is accepted and returned as nil (the caller infers it).
func (c *checker) resolve(te *syntax.TypeExpr, allowStatic bool) *types.Type {
	switch te.Kind {
	case syntax.TStr:
		n := c.constInt(te.N)
		if !n.IsInt64() || n.Sign() < 0 || n.Int64() > 1<<24 {
			errf("E0307", te.Pos, "invalid Str capacity %s", n)
		}
		return types.Str(int(n.Int64()))
	case syntax.TArray:
		el := c.resolve(te.Elem, false)
		n := c.constInt(te.N)
		if !n.IsInt64() || n.Sign() <= 0 || n.Int64() > 1<<24 {
			errf("E0307", te.Pos, "invalid array length %s", n)
		}
		return types.Array(el, int(n.Int64()))
	}
	if t, ok := c.typeSubst[te.Name]; ok {
		return t
	}
	switch te.Name {
	case "bool":
		return types.Bool
	case "StaticStr":
		if allowStatic {
			return nil
		}
		errf("E0305", te.Pos, "`StaticStr` is only allowed on const and let declarations")
	}
	if t := types.Int(te.Name); t != nil {
		return t
	}
	if t := types.Float(te.Name); t != nil {
		return t
	}
	if t := c.info.Structs[te.Name]; t != nil {
		return t
	}
	if t := c.info.Enums[te.Name]; t != nil {
		return t
	}
	errf("E0004", te.Pos, "unknown type `%s`", te.Name)
	return nil
}

// constInt evaluates a compile-time integer expression used in type positions.
func (c *checker) constInt(e syntax.Expr) *big.Int {
	switch n := e.(type) {
	case *syntax.IntLit:
		return n.V
	case *syntax.Cast:
		return c.constInt(n.X)
	case *syntax.Unary:
		if n.Op == "-" {
			return new(big.Int).Neg(c.constInt(n.X))
		}
	case *syntax.Ident:
		if v, ok := c.constSubst[n.Name]; ok {
			return v
		}
		if v, ok := c.opts.Defines[n.Name]; ok && c.info.Consts[n.Name] != nil {
			return v
		}
		cd := c.info.Consts[n.Name]
		if cd != nil {
			if c.visit[n.Name] {
				errf("E0307", n.Pos, "const `%s` depends on itself", n.Name)
			}
			c.visit[n.Name] = true
			defer delete(c.visit, n.Name)
			return c.constInt(cd.X)
		}
	case *syntax.Call:
		if len(n.Args) == 1 {
			if id, ok := n.Args[0].(*syntax.Ident); ok {
				switch n.Name {
				case "sizeof":
					return big.NewInt(int64(c.sizeofType(id).Size()))
				case "alignof":
					return big.NewInt(int64(c.sizeofType(id).Align()))
				case "bits_of":
					return big.NewInt(int64(c.bitsOf(id)))
				case "max_of", "min_of":
					t := c.sizeofType(id)
					if !t.IsInt() {
						errf("E0305", id.Pos, "`%s` needs an integer type", n.Name)
					}
					lo, hi := t.Range()
					if n.Name == "max_of" {
						return hi
					}
					return lo
				}
			}
		}
	case *syntax.Binary:
		a, b := c.constInt(n.L), c.constInt(n.R)
		r := new(big.Int)
		switch n.Op {
		case "+":
			return r.Add(a, b)
		case "-":
			return r.Sub(a, b)
		case "*":
			return r.Mul(a, b)
		case "/", "%":
			if b.Sign() == 0 {
				errf("E0303", n.Pos, "division by zero in constant expression")
			}
			if n.Op == "/" {
				return r.Quo(a, b)
			}
			return r.Rem(a, b)
		case "<<":
			return r.Lsh(a, uint(b.Uint64()))
		case ">>":
			return r.Rsh(a, uint(b.Uint64()))
		}
	}
	errf("E0307", e.Position(), "expected a compile-time integer constant")
	return nil
}

func (c *checker) sizeofType(id *syntax.Ident) *types.Type {
	if t, ok := c.typeSubst[id.Name]; ok {
		return t
	}
	if t := types.Int(id.Name); t != nil {
		return t
	}
	if id.Name == "bool" {
		return types.Bool
	}
	if t := types.Float(id.Name); t != nil {
		return t
	}
	if t := c.info.Structs[id.Name]; t != nil {
		return t
	}
	if t := c.info.Enums[id.Name]; t != nil {
		return t
	}
	errf("E0004", id.Pos, "unknown type `%s`", id.Name)
	return nil
}

func (c *checker) bitsOf(id *syntax.Ident) int {
	t := c.sizeofType(id)
	switch t.Kind {
	case types.KInt, types.KEnum:
		return t.Bits
	case types.KBool:
		return 1
	}
	errf("E0305", id.Pos, "`bits_of` needs a scalar type, found `%s`", t)
	return 0
}

func (c *checker) checkConst(cd *syntax.ConstDecl) {
	if c.constOK[cd.Name] {
		return
	}
	var want *types.Type
	if cd.TE != nil {
		want = c.resolve(cd.TE, true)
	}
	t := c.infer(cd.X, want)
	if want != nil {
		c.coerce(cd.X, want)
		t = want
	} else {
		c.finalize(cd.X)
		t = cd.X.Type()
	}
	cd.Ty = t
	c.constOK[cd.Name] = true
}

func (c *checker) checkFn(f *syntax.FnDecl) {
	c.fn = f
	c.push()
	for _, p := range f.Params {
		c.declare(p.Name, p.Ty, false)
	}
	c.block(f.Body)
	c.pop()
	if f.Ret.Kind != types.KUnit && !terminates(f.Body) {
		errf("E0306", f.Pos, "function `%s` may finish without returning a value", f.Name)
	}
	c.fn = nil
}

func terminates(b []syntax.Stmt) bool {
	if len(b) == 0 {
		return false
	}
	switch s := b[len(b)-1].(type) {
	case *syntax.Return:
		return true
	case *syntax.If:
		return len(s.Else) > 0 && terminates(s.Then) && terminates(s.Else)
	case *syntax.Region:
		return terminates(s.Body)
	case *syntax.Block:
		return terminates(s.Body)
	case *syntax.MatchStmt:
		for _, a := range s.Arms {
			if !terminates(a.Body) {
				return false
			}
		}
		return true
	}
	return false
}

func (c *checker) block(b []syntax.Stmt) {
	c.push()
	for _, s := range b {
		c.stmt(s)
	}
	c.pop()
}

func (c *checker) stmt(s syntax.Stmt) {
	switch s := s.(type) {
	case *syntax.Let:
		var want *types.Type
		if s.TE != nil {
			want = c.resolve(s.TE, true)
		}
		t := c.infer(s.Init, want)
		if t.Kind == types.KUnit {
			errf("E0305", s.Pos, "cannot bind a value of type `()`")
		}
		if want != nil {
			c.coerce(s.Init, want)
			t = want
		} else {
			c.finalize(s.Init)
			t = s.Init.Type()
		}
		s.Ty = t
		c.declare(s.Name, t, s.Mut)
	case *syntax.Assign:
		tt := c.infer(s.Target, nil)
		if ix, ok := s.Target.(*syntax.Index); ok && ix.X.Type().Kind == types.KStr {
			errf("E0305", s.Pos, "strings are immutable: cannot assign to a string element")
		}
		c.requireMutable(s.Target)
		c.infer(s.X, tt)
		c.coerce(s.X, tt)
	case *syntax.ExprStmt:
		c.infer(s.X, nil)
		c.finalize(s.X)
	case *syntax.If:
		c.cond(s.Cond)
		c.block(s.Then)
		c.block(s.Else)
	case *syntax.While:
		c.cond(s.Cond)
		if s.Bound < 0 {
			errf("E0201", s.Pos, "`while` requires `bound(N)`")
		}
		c.loopDepth++
		c.block(s.Body)
		c.loopDepth--
	case *syntax.For:
		lt := c.infer(s.Lo, nil)
		ht := c.infer(s.Hi, nil)
		var t *types.Type
		switch {
		case lt.Kind == types.KUntyped && ht.Kind == types.KUntyped:
			t = types.I32
		case lt.Kind == types.KUntyped:
			t = ht
		default:
			t = lt
		}
		if !t.IsInt() {
			errf("E0305", s.Pos, "`for` range bounds must be integers, found `%s`", t)
		}
		c.coerce(s.Lo, t)
		c.coerce(s.Hi, t)
		s.Ty = t
		c.push()
		c.declare(s.Var, t, false)
		c.loopDepth++
		c.block(s.Body)
		c.loopDepth--
		c.pop()
	case *syntax.Return:
		if s.X == nil {
			if c.fn.Ret.Kind != types.KUnit {
				errf("E0305", s.Pos, "expected a return value of type `%s`", c.fn.Ret)
			}
			return
		}
		if c.fn.Ret.Kind == types.KUnit {
			errf("E0305", s.Pos, "function `%s` returns `()`, found a value", c.fn.Name)
		}
		c.infer(s.X, c.fn.Ret)
		c.coerce(s.X, c.fn.Ret)
	case *syntax.Block:
		c.block(s.Body)
	case *syntax.Break:
		if c.loopDepth == 0 {
			errf("E0311", s.Pos, "`break` outside of a loop")
		}
	case *syntax.Continue:
		if c.loopDepth == 0 {
			errf("E0311", s.Pos, "`continue` outside of a loop")
		}
	case *syntax.MatchStmt:
		c.match(s.X, s.Arms, false, nil)
	case *syntax.Region:
		if c.info.Arenas[s.Arena] == nil {
			errf("E0004", s.Pos, "unknown arena `%s`", s.Arena)
		}
		if c.fn.Pure {
			errf("E0102", s.Pos, "`region` allocates and cannot appear in a `pure fn`")
		}
		c.block(s.Body)
	}
}

func (c *checker) cond(e syntax.Expr) {
	t := c.infer(e, types.Bool)
	if t.Kind != types.KBool {
		errf("E0305", e.Position(), "expected `bool`, found `%s`", t)
	}
}

func (c *checker) requireMutable(e syntax.Expr) {
	switch n := e.(type) {
	case *syntax.Ident:
		l := c.lookup(n.Name)
		if l == nil {
			errf("E0004", n.Pos, "cannot assign to `%s`: not a local variable", n.Name)
		}
		if !l.mut {
			errh("E0310", n.Pos, "declare it with `let mut`", "cannot assign to immutable variable `%s`", n.Name)
		}
	case *syntax.Index:
		c.requireMutable(n.X)
	case *syntax.Field:
		c.requireMutable(n.X)
	}
}

func isUntyped(t *types.Type) bool { return t.Kind == types.KUntyped || t.Kind == types.KUntypedF }

// coerce checks that e (already inferred) can be used as type to, fixing literal types.
func (c *checker) coerce(e syntax.Expr, to *types.Type) {
	from := e.Type()
	switch from.Kind {
	case types.KUntyped:
		if to.Kind != types.KInt && to.Kind != types.KFloat {
			errf("E0305", e.Position(), "mismatched types: expected `%s`, found integer", to)
		}
		c.setUntyped(e, to)
		return
	case types.KUntypedF:
		if to.Kind != types.KFloat {
			errf("E0305", e.Position(), "mismatched types: expected `%s`, found a floating-point value", to)
		}
		c.setUntyped(e, to)
		return
	}
	if !types.Assignable(from, to) {
		errf("E0305", e.Position(), "mismatched types: expected `%s`, found `%s`", to, from)
	}
}

func (c *checker) finalize(e syntax.Expr) {
	if t := e.Type(); t != nil {
		switch t.Kind {
		case types.KUntyped:
			c.setUntyped(e, types.I32)
		case types.KUntypedF:
			c.setUntyped(e, types.F64)
		}
	}
}

// floatLiteral parses decimal text as the given float type, rejecting overflow.
func floatLiteral(text string, bits int) (float64, bool) {
	v, err := strconv.ParseFloat(text, bits)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, false
	}
	return v, true
}

func (c *checker) setUntyped(e syntax.Expr, to *types.Type) {
	if e.Type() == nil || !isUntyped(e.Type()) {
		return
	}
	switch n := e.(type) {
	case *syntax.IntLit:
		if to.Kind == types.KFloat {
			f := new(big.Float).SetInt(n.V)
			if v, acc := f.Float64(); acc != big.Exact || (to.Bits == 32 && float64(float32(v)) != v) {
				errh("E0301", n.Pos, "write it as a float literal, e.g. `1.0`", "integer literal %s is not exactly representable in `%s`", n.V, to)
			}
		} else {
			lo, hi := to.Range()
			if n.V.Cmp(lo) < 0 || n.V.Cmp(hi) > 0 {
				errf("E0301", n.Pos, "literal %s does not fit in `%s`", n.V, to)
			}
		}
	case *syntax.FloatLit:
		if _, ok := floatLiteral(n.Text, to.Bits); !ok {
			errf("E0301", n.Pos, "float literal %s is out of range for `%s`", n.Text, to)
		}
	case *syntax.Unary:
		if to.Kind == types.KInt && !to.Signed {
			errf("E0301", n.Pos, "cannot negate an unsigned value of type `%s`", to)
		}
		c.setUntyped(n.X, to)
	case *syntax.Binary:
		c.setUntyped(n.L, to)
		if n.Op != "<<" && n.Op != ">>" {
			c.setUntyped(n.R, to)
		}
	case *syntax.Static:
		c.setUntyped(n.X, to)
	case *syntax.MatchExpr:
		for _, a := range n.Arms {
			c.setUntyped(a.Val, to)
		}
	case *syntax.Call:
		for _, a := range n.Args {
			c.setUntyped(a, to)
		}
	}
	e.SetType(to)
}

func isArith(op string) bool {
	switch op {
	case "+", "-", "*", "/", "%", "&", "|", "^":
		return true
	}
	return false
}

// unify makes the two operand types agree, returning the common type (possibly untyped).
func (c *checker) unify(l, r syntax.Expr) *types.Type {
	lt, rt := l.Type(), r.Type()
	lu, ru := isUntyped(lt), isUntyped(rt)
	switch {
	case lu && ru:
		if lt.Kind == types.KUntypedF || rt.Kind == types.KUntypedF {
			return types.UntypedF
		}
		return types.Untyped
	case lu:
		c.checkLiteralFits(l, rt)
		c.setUntyped(l, rt)
		return rt
	case ru:
		c.checkLiteralFits(r, lt)
		c.setUntyped(r, lt)
		return lt
	}
	if !types.Equal(lt, rt) {
		errf("E0305", r.Position(), "mismatched types: `%s` and `%s`", lt, rt)
	}
	return lt
}

// checkLiteralFits verifies an untyped literal can take the typed operand's type.
func (c *checker) checkLiteralFits(lit syntax.Expr, to *types.Type) {
	k := lit.Type().Kind
	if (k == types.KUntyped && to.Kind != types.KInt && to.Kind != types.KFloat) ||
		(k == types.KUntypedF && to.Kind != types.KFloat) {
		errf("E0305", lit.Position(), "mismatched types: `%s` and a literal of another kind", to)
	}
}

func (c *checker) infer(e syntax.Expr, want *types.Type) *types.Type {
	t := c.infer0(e, want)
	e.SetType(t)
	return t
}

func (c *checker) infer0(e syntax.Expr, want *types.Type) *types.Type {
	switch n := e.(type) {
	case *syntax.IntLit:
		return types.Untyped
	case *syntax.FloatLit:
		return types.UntypedF
	case *syntax.StrLit:
		return types.Str(len(n.V))
	case *syntax.BoolLit:
		return types.Bool
	case *syntax.Ident:
		if l := c.lookup(n.Name); l != nil {
			return l.ty
		}
		if v, ok := c.constSubst[n.Name]; ok {
			n.EnumVal = v // a const generic parameter behaves like a typed constant
			return c.constTy[n.Name]
		}
		if cd := c.info.Consts[n.Name]; cd != nil {
			c.checkConst(cd)
			return cd.Ty
		}
		if i := strings.Index(n.Name, "::"); i > 0 {
			if et := c.info.Enums[n.Name[:i]]; et != nil {
				vi := et.VariantIndex(n.Name[i+2:])
				if vi < 0 {
					errf("E0004", n.Pos, "enum `%s` has no variant `%s`", et.Name, n.Name[i+2:])
				}
				n.EnumVal = big.NewInt(et.Variants[vi].Value)
				return et
			}
		}
		errf("E0004", n.Pos, "unknown name `%s`", n.Name)
	case *syntax.Unary:
		if n.Op == "!" {
			t := c.infer(n.X, types.Bool)
			if t.Kind != types.KBool {
				errf("E0305", n.Pos, "`!` needs a bool, found `%s`", t)
			}
			return types.Bool
		}
		t := c.infer(n.X, want)
		if isUntyped(t) {
			return t
		}
		if t.Kind == types.KFloat {
			return t
		}
		if !t.IsInt() || !t.Signed {
			errf("E0305", n.Pos, "cannot negate `%s`", t)
		}
		return t
	case *syntax.Binary:
		return c.binary(n, want)
	case *syntax.Cast:
		to := c.resolve(n.To, false)
		t := c.infer(n.X, nil)
		if t.Kind == types.KUntyped {
			if to.Kind == types.KFloat {
				c.setUntyped(n.X, to)
				t = to
			} else {
				c.setUntyped(n.X, types.I64)
				t = types.I64
			}
		} else if t.Kind == types.KUntypedF {
			c.setUntyped(n.X, types.F64)
			t = types.F64
		}
		if !(t.IsScalar() && to.IsScalar()) || to.Kind == types.KBool || to.Kind == types.KEnum ||
			(t.Kind == types.KBool && to.Kind == types.KFloat) || (t.Kind == types.KEnum && to.Kind == types.KFloat) {
			errf("E0305", n.Pos, "cannot cast `%s` as `%s`", t, to)
		}
		return to
	case *syntax.Index:
		xt := c.infer(n.X, nil)
		if xt.Kind != types.KArray && xt.Kind != types.KStr {
			errf("E0305", n.Pos, "cannot index into `%s`", xt)
		}
		it := c.infer(n.I, nil)
		if it.Kind == types.KUntyped {
			if il, ok := n.I.(*syntax.IntLit); ok && il.V.Sign() < 0 {
				errf("E0304", n.Pos, "negative array index")
			}
			c.setUntyped(n.I, types.U64)
		} else if !it.IsInt() {
			errf("E0305", n.I.Position(), "array index must be an integer, found `%s`", it)
		}
		if xt.Kind == types.KStr {
			return types.U8
		}
		return xt.Elem
	case *syntax.Field:
		xt := c.infer(n.X, nil)
		if xt.Kind != types.KStruct {
			errf("E0305", n.Pos, "`%s` has no fields", xt)
		}
		i := xt.FieldIndex(n.Name)
		if i < 0 {
			errf("E0004", n.Pos, "struct `%s` has no field `%s`", xt.Name, n.Name)
		}
		n.Index = i
		return xt.Fields[i].Ty
	case *syntax.StructLit:
		st := c.info.Structs[n.Name]
		if st == nil {
			errf("E0004", n.Pos, "unknown struct `%s`", n.Name)
		}
		seen := map[string]bool{}
		for _, fi := range n.Fields {
			i := st.FieldIndex(fi.Name)
			if i < 0 {
				errf("E0004", fi.Pos, "struct `%s` has no field `%s`", n.Name, fi.Name)
			}
			if seen[fi.Name] {
				errf("E0004", fi.Pos, "field `%s` specified twice", fi.Name)
			}
			seen[fi.Name] = true
			ft := st.Fields[i].Ty
			c.infer(fi.X, ft)
			c.coerce(fi.X, ft)
		}
		for _, f := range st.Fields {
			if !seen[f.Name] {
				errf("E0004", n.Pos, "missing field `%s` in initializer of `%s`", f.Name, n.Name)
			}
		}
		return st
	case *syntax.ArrayLit:
		var elemWant *types.Type
		if want != nil && want.Kind == types.KArray {
			elemWant = want.Elem
		}
		var et *types.Type
		for _, x := range n.Elems {
			c.infer(x, elemWant)
		}
		anyFloat := false
		for _, x := range n.Elems {
			if x.Type().Kind == types.KUntypedF {
				anyFloat = true
			}
			if !isUntyped(x.Type()) {
				et = x.Type()
				break
			}
		}
		if et == nil {
			et = types.I32
			if anyFloat {
				et = types.F64
			}
			if elemWant != nil && (elemWant.IsInt() || elemWant.IsFloat()) {
				et = elemWant
			}
		}
		for _, x := range n.Elems {
			if x.Type().Kind == types.KStr && et.Kind == types.KStr {
				if x.Type().Cap > et.Cap {
					et = x.Type()
				}
			}
		}
		if elemWant != nil && elemWant.Kind == types.KStr && et.Kind == types.KStr && elemWant.Cap >= et.Cap {
			et = elemWant // a string array literal takes the capacity it is expected to have
		}
		for _, x := range n.Elems {
			c.coerce(x, et)
		}
		return types.Array(et, len(n.Elems))
	case *syntax.ArrayRep:
		var elemWant *types.Type
		if want != nil && want.Kind == types.KArray {
			elemWant = want.Elem
		}
		c.infer(n.Val, elemWant)
		et := n.Val.Type()
		if isUntyped(et) {
			et = types.I32
			if et0 := n.Val.Type(); et0.Kind == types.KUntypedF {
				et = types.F64
			}
			if elemWant != nil && (elemWant.IsInt() || elemWant.IsFloat()) {
				et = elemWant
			}
			c.coerce(n.Val, et)
		}
		if elemWant != nil && elemWant.Kind == types.KStr && et.Kind == types.KStr && elemWant.Cap >= et.Cap {
			et = elemWant
		}
		ln := c.constInt(n.N)
		if !ln.IsInt64() || ln.Sign() <= 0 || ln.Int64() > 1<<24 {
			errf("E0307", n.Pos, "invalid array length %s", ln)
		}
		n.Len = int(ln.Int64())
		c.infer(n.N, nil)
		c.finalize(n.N)
		return types.Array(et, n.Len)
	case *syntax.Static:
		return c.infer(n.X, want)
	case *syntax.MatchExpr:
		return c.match(n.X, n.Arms, true, want)
	case *syntax.Call:
		return c.call(n, want)
	}
	errf("E0000", e.Position(), "internal: unhandled expression %T", e)
	return nil
}

func (c *checker) binary(n *syntax.Binary, want *types.Type) *types.Type {
	switch n.Op {
	case "&&", "||":
		for _, x := range []syntax.Expr{n.L, n.R} {
			if t := c.infer(x, types.Bool); t.Kind != types.KBool {
				errf("E0305", x.Position(), "`%s` needs bool operands, found `%s`", n.Op, t)
			}
		}
		return types.Bool
	case "++":
		for _, x := range []syntax.Expr{n.L, n.R} {
			if t := c.infer(x, nil); t.Kind != types.KStr {
				errh("E0302", x.Position(), "wrap numbers with `str(...)`", "`++` needs strings, found `%s`", t)
			}
		}
		return types.Str(n.L.Type().Cap + n.R.Type().Cap)
	case "<<", ">>":
		lt := c.infer(n.L, want)
		rt := c.infer(n.R, nil)
		if lt.Kind == types.KFloat || lt.Kind == types.KUntypedF {
			errf("E0305", n.Pos, "shifts need integer operands")
		}
		if rt.Kind == types.KUntyped {
			c.setUntyped(n.R, types.U32)
			rt = types.U32
		}
		if (lt.Kind != types.KInt && lt.Kind != types.KUntyped) || !rt.IsInt() {
			errf("E0305", n.Pos, "shift needs integer operands")
		}
		return lt
	case "==", "!=", "<", "<=", ">", ">=":
		c.infer(n.L, nil)
		c.infer(n.R, nil)
		if n.L.Type().Kind == types.KStr && n.R.Type().Kind == types.KStr {
			if n.Op != "==" && n.Op != "!=" {
				errf("E0305", n.Pos, "strings only support `==` and `!=`")
			}
			return types.Bool
		}
		t := c.unify(n.L, n.R)
		if t.Kind == types.KUntyped {
			c.setUntyped(n.L, types.I32)
			c.setUntyped(n.R, types.I32)
			t = types.I32
		} else if t.Kind == types.KUntypedF {
			c.setUntyped(n.L, types.F64)
			c.setUntyped(n.R, types.F64)
			t = types.F64
		}
		if (t.Kind == types.KBool || t.Kind == types.KEnum) && (n.Op == "==" || n.Op == "!=") {
			return types.Bool
		}
		if t.Kind == types.KFloat {
			return types.Bool
		}
		if !t.IsInt() {
			errf("E0305", n.Pos, "cannot compare values of type `%s`", t)
		}
		return types.Bool
	}
	var w *types.Type
	if want != nil && want.IsInt() {
		w = want
	}
	if want != nil && want.IsFloat() {
		w = want
	}
	c.infer(n.L, w)
	c.infer(n.R, w)
	t := c.unify(n.L, n.R)
	if t.Kind == types.KFloat || t.Kind == types.KUntypedF {
		if n.Op != "+" && n.Op != "-" && n.Op != "*" && n.Op != "/" {
			errf("E0305", n.Pos, "operator `%s` is not defined for floating-point values", n.Op)
		}
		return t
	}
	if t.Kind != types.KUntyped && !t.IsInt() {
		errf("E0305", n.Pos, "operator `%s` needs integer operands, found `%s`", n.Op, t)
	}
	return t
}

var ioBuiltins = map[string]bool{
	"io::println": true, "io::print": true, "io::read_line": true,
	"io::read_u32": true, "io::read_u64": true, "io::read_i32": true, "io::read_i64": true,
	"io::read_f32": true, "io::read_f64": true,
}

func (c *checker) needArgs(n *syntax.Call, k int) {
	if len(n.Args) != k {
		errf("E0005", n.Pos, "`%s` expects %d argument(s), found %d", n.Name, k, len(n.Args))
	}
}

func (c *checker) call(n *syntax.Call, want *types.Type) *types.Type {
	if ioBuiltins[n.Name] {
		if c.fn != nil && (c.fn.Pure || !c.fn.IO) {
			errf("E0102", n.Pos, "`%s` performs I/O; the enclosing function must be declared `io`", n.Name)
		}
	}
	switch n.Name {
	case "io::println", "io::print":
		c.needArgs(n, 1)
		if t := c.infer(n.Args[0], nil); t.Kind != types.KStr {
			errh("E0305", n.Pos, "use `str(x)` for numbers", "`%s` needs a string, found `%s`", n.Name, t)
		}
		return types.Unit
	case "io::read_line":
		c.needArgs(n, 1)
		c.infer(n.Args[0], nil)
		k := c.constInt(n.Args[0])
		if !k.IsInt64() || k.Int64() < 1 || k.Int64() > 1<<20 {
			errf("E0307", n.Pos, "read_line capacity must be between 1 and 1048576")
		}
		c.coerce(n.Args[0], types.U64)
		return types.Str(int(k.Int64()))
	case "io::read_u32", "io::read_u64", "io::read_i32", "io::read_i64":
		c.needArgs(n, 0)
		return types.Int(strings.TrimPrefix(n.Name, "io::read_"))
	case "io::read_f32", "io::read_f64":
		c.needArgs(n, 0)
		return types.Float(strings.TrimPrefix(n.Name, "io::read_"))
	case "sqrt", "floor", "ceil", "trunc", "abs", "is_nan":
		c.needArgs(n, 1)
		w := types.F64
		if want != nil && want.IsFloat() {
			w = want
		}
		var t *types.Type
		if n.Name == "abs" && (want == nil || want.IsInt()) {
			// no float expected: let an integer argument stay an integer
			t = c.infer(n.Args[0], want)
		} else {
			t = c.infer(n.Args[0], w)
		}
		if n.Name == "abs" && t.Kind == types.KUntyped && !(want != nil && want.IsFloat()) {
			it := types.I32 // an integer literal stays an integer
			if want != nil && want.IsInt() {
				if lo, _ := want.Range(); lo.Sign() < 0 {
					it = want
				}
			}
			c.coerce(n.Args[0], it)
			return it
		}
		if n.Name == "abs" && t.IsInt() {
			if lo, _ := t.Range(); lo.Sign() >= 0 {
				errf("E0305", n.Pos, "`abs` needs a signed integer or a floating-point value, found `%s`", t)
			}
			return t
		}
		if isUntyped(t) {
			c.coerce(n.Args[0], w)
			t = w
		}
		if !t.IsFloat() {
			errf("E0305", n.Pos, "`%s` needs a floating-point value, found `%s`", n.Name, t)
		}
		if n.Name == "is_nan" {
			return types.Bool
		}
		return t
	case "str":
		c.needArgs(n, 1)
		t := c.infer(n.Args[0], nil)
		if isUntyped(t) {
			c.finalize(n.Args[0])
			t = n.Args[0].Type()
		}
		if t.Kind == types.KBool {
			return types.Str(5) // "true" or "false"
		}
		if !t.IsInt() && !t.IsFloat() {
			errf("E0305", n.Pos, "`str` needs a number or a bool, found `%s`", t)
		}
		return types.Str(t.DecimalDigits())
	case "len":
		c.needArgs(n, 1)
		t := c.infer(n.Args[0], nil)
		switch t.Kind {
		case types.KStr:
			return types.U32
		case types.KArray:
			return types.U64
		}
		errf("E0305", n.Pos, "`len` needs a string or array, found `%s`", t)
	case "clamp", "require":
		c.needArgs(n, 3)
		for _, a := range n.Args {
			c.infer(a, want)
		}
		var t *types.Type
		for _, a := range n.Args {
			if a.Type().Kind != types.KUntyped {
				t = a.Type()
				break
			}
		}
		if t == nil {
			if want != nil && want.IsInt() {
				t = want
			} else {
				for _, a := range n.Args {
					if a.Type().Kind == types.KUntypedF {
						errf("E0305", n.Pos, "`%s` needs integers, found a floating-point value", n.Name)
					}
				}
				return types.Untyped // all literals: let the context decide
			}
		}
		if !t.IsInt() {
			errf("E0305", n.Pos, "`%s` needs integers, found `%s`", n.Name, t)
		}
		for _, a := range n.Args {
			c.coerce(a, t)
		}
		return t
	case "min", "max":
		c.needArgs(n, 2)
		c.infer(n.Args[0], want)
		c.infer(n.Args[1], want)
		t := c.unify(n.Args[0], n.Args[1])
		if !isUntyped(t) && !t.IsInt() && !t.IsFloat() {
			errf("E0305", n.Pos, "`%s` needs numbers, found `%s`", n.Name, t)
		}
		return t
	case "alloc":
		if c.fn != nil && c.fn.Pure {
			errf("E0102", n.Pos, "`alloc` cannot appear in a `pure fn`")
		}
		c.needArgs(n, 2)
		id, ok := n.Args[0].(*syntax.Ident)
		if !ok || c.info.Arenas[id.Name] == nil {
			errf("E0004", n.Pos, "first argument of `alloc` must be an arena name")
		}
		id.SetType(types.Ptr)
		t := c.infer(n.Args[1], nil)
		if t.Kind == types.KUntyped {
			c.setUntyped(n.Args[1], types.U64)
		} else if !t.IsInt() {
			errf("E0305", n.Pos, "allocation size must be an integer, found `%s`", t)
		}
		return types.Ptr
	case "sizeof":
		c.needArgs(n, 1)
		id, ok := n.Args[0].(*syntax.Ident)
		if !ok {
			errf("E0004", n.Pos, "`sizeof` needs a type name")
		}
		id.SetType(c.sizeofType(id))
		return types.U64
	case "static_assert":
		if len(n.Args) != 1 && len(n.Args) != 2 {
			errf("E0005", n.Pos, "`static_assert` expects a condition and an optional message")
		}
		c.cond(n.Args[0])
		if len(n.Args) == 2 {
			if _, ok := n.Args[1].(*syntax.StrLit); !ok {
				errf("E0305", n.Pos, "the `static_assert` message must be a string literal")
			}
			c.infer(n.Args[1], nil)
		}
		return types.Unit
	case "static_print":
		c.needArgs(n, 1)
		if t := c.infer(n.Args[0], nil); t.Kind != types.KStr {
			errf("E0305", n.Pos, "`static_print` needs a string, found `%s`", t)
		}
		return types.Unit
	case "alignof", "bits_of", "max_of", "min_of":
		c.needArgs(n, 1)
		id, ok := n.Args[0].(*syntax.Ident)
		if !ok {
			errf("E0004", n.Pos, "`%s` needs a type name", n.Name)
		}
		t := c.sizeofType(id)
		id.SetType(t)
		switch n.Name {
		case "max_of", "min_of":
			if !t.IsInt() {
				errf("E0305", n.Pos, "`%s` needs an integer type, found `%s`", n.Name, t)
			}
			return t
		case "bits_of":
			c.bitsOf(id)
		}
		return types.U64
	case "offsetof":
		c.needArgs(n, 2)
		id, ok1 := n.Args[0].(*syntax.Ident)
		fld, ok2 := n.Args[1].(*syntax.Ident)
		if !ok1 || !ok2 {
			errf("E0004", n.Pos, "`offsetof` needs a struct name and a field name")
		}
		t := c.sizeofType(id)
		if t.Kind != types.KStruct || t.FieldIndex(fld.Name) < 0 {
			errf("E0004", n.Pos, "`%s` has no field `%s`", id.Name, fld.Name)
		}
		id.SetType(t)
		fld.SetType(types.U64)
		return types.U64
	case "bytes":
		c.needArgs(n, 1)
		t := c.infer(n.Args[0], nil)
		if t.Kind != types.KStr || t.Cap == 0 {
			errf("E0305", n.Pos, "`bytes` needs a non-empty string, found `%s`", t)
		}
		return types.Array(types.U8, t.Cap)
	case "embed":
		c.needArgs(n, 1)
		lit, ok := n.Args[0].(*syntax.StrLit)
		if !ok {
			errf("E0309", n.Pos, "`embed` needs a string literal path")
		}
		c.infer(lit, nil)
		n.Embed = c.readEmbed(n.Pos, lit.V)
		return types.Array(types.U8, len(n.Embed))
	}
	f := c.info.Fns[n.Name]
	if f == nil {
		errf("E0004", n.Pos, "unknown function `%s`", n.Name)
	}
	generic := len(f.Generics) > 0
	if generic {
		f = c.instantiateFor(n, f)
	} else if len(n.TypeArgs) > 0 {
		errf("E0313", n.Pos, "`%s` is not generic", n.Name)
	}
	if len(n.Args) != len(f.Params) {
		errf("E0005", n.Pos, "`%s` expects %d argument(s), found %d", n.Name, len(f.Params), len(n.Args))
	}
	if c.fn != nil {
		if c.fn.Pure && !f.Pure {
			errf("E0102", n.Pos, "`pure fn %s` cannot call non-pure `%s`", c.fn.Name, f.Name)
		}
		if !c.fn.IO && f.IO {
			errf("E0102", n.Pos, "`%s` is an `io` function; the caller must be declared `io`", f.Name)
		}
	}
	for i, a := range n.Args {
		if !generic {
			c.infer(a, f.Params[i].Ty)
		}
		c.coerce(a, f.Params[i].Ty)
	}
	return f.Ret
}

// checkRecursion requires bound(rec = N) on every function that can reach itself.
func (c *checker) checkRecursion() {
	calls := map[string][]string{}
	for _, f := range c.info.Prog.Fns {
		if len(f.Generics) > 0 {
			continue
		}
		seen := map[string]bool{}
		var walk func(e syntax.Expr)
		walk = func(e syntax.Expr) {
			switch n := e.(type) {
			case *syntax.Call:
				if c.info.Fns[n.Name] != nil && !seen[n.Name] {
					seen[n.Name] = true
					calls[f.Name] = append(calls[f.Name], n.Name)
				}
				for _, a := range n.Args {
					walk(a)
				}
			case *syntax.Unary:
				walk(n.X)
			case *syntax.Binary:
				walk(n.L)
				walk(n.R)
			case *syntax.Cast:
				walk(n.X)
			case *syntax.Index:
				walk(n.X)
				walk(n.I)
			case *syntax.Field:
				walk(n.X)
			case *syntax.Static:
				walk(n.X)
			case *syntax.ArrayLit:
				for _, x := range n.Elems {
					walk(x)
				}
			case *syntax.ArrayRep:
				walk(n.Val)
			case *syntax.StructLit:
				for _, fi := range n.Fields {
					walk(fi.X)
				}
			case *syntax.MatchExpr:
				walk(n.X)
				for _, a := range n.Arms {
					walk(a.Val)
				}
			}
		}
		var ws func(b []syntax.Stmt)
		ws = func(b []syntax.Stmt) {
			for _, s := range b {
				switch s := s.(type) {
				case *syntax.Let:
					walk(s.Init)
				case *syntax.Assign:
					walk(s.Target)
					walk(s.X)
				case *syntax.ExprStmt:
					walk(s.X)
				case *syntax.If:
					walk(s.Cond)
					ws(s.Then)
					ws(s.Else)
				case *syntax.While:
					walk(s.Cond)
					ws(s.Body)
				case *syntax.For:
					walk(s.Lo)
					walk(s.Hi)
					ws(s.Body)
				case *syntax.Return:
					if s.X != nil {
						walk(s.X)
					}
				case *syntax.Region:
					ws(s.Body)
				case *syntax.Block:
					ws(s.Body)
				case *syntax.MatchStmt:
					walk(s.X)
					for _, a := range s.Arms {
						ws(a.Body)
					}
				}
			}
		}
		ws(f.Body)
	}
	for _, f := range c.info.Prog.Fns {
		if len(f.Generics) > 0 {
			continue
		}
		reach := map[string]bool{}
		var dfs func(string)
		dfs = func(n string) {
			for _, m := range calls[n] {
				if !reach[m] {
					reach[m] = true
					dfs(m)
				}
			}
		}
		dfs(f.Name)
		if reach[f.Name] && f.BoundRec < 0 {
			errf("E0201", f.Pos, "recursive function `%s` requires `bound(rec = N)`", f.Name)
		}
	}
}

var _ = fmt.Sprintf

// readEmbed loads a build-time file; the path is resolved against BaseDir and may not escape it.
func (c *checker) readEmbed(pos diag.Pos, rel string) []byte {
	if c.opts.BaseDir == "" {
		errh("E0309", pos, "compile from a file (apus build/run FILE) so paths have a base directory", "`embed` has no source directory to resolve against")
	}
	if filepath.IsAbs(rel) || strings.Contains(filepath.ToSlash(filepath.Clean(rel)), "..") {
		errh("E0309", pos, "use a path inside the source directory", "embed path `%s` escapes the source directory", rel)
	}
	data, err := os.ReadFile(filepath.Join(c.opts.BaseDir, rel))
	if err != nil {
		errf("E0309", pos, "cannot read embedded file: %v", err)
	}
	if len(data) == 0 || len(data) > 1<<20 {
		errf("E0309", pos, "embedded file must be between 1 byte and 1 MiB (is %d bytes)", len(data))
	}
	return data
}

// match checks a match statement (isExpr=false, arm bodies are statements) or
// expression (arm values are unified). It resolves patterns and proves exhaustiveness.
func (c *checker) match(x syntax.Expr, arms []*syntax.Arm, isExpr bool, want *types.Type) *types.Type {
	st := c.infer(x, nil)
	if st.Kind == types.KUntyped {
		c.finalize(x)
		st = types.I32
	}
	if !st.IsScalar() || st.Kind == types.KFloat {
		errf("E0305", x.Position(), "cannot match on a value of type `%s`", st)
	}
	lo, hi := st.Range()
	type iv struct{ lo, hi *big.Int }
	var cover []iv
	wild := false
	for _, a := range arms {
		for _, p := range a.Pats {
			if p.Wild {
				wild = true
				continue
			}
			switch st.Kind {
			case types.KEnum:
				id, ok := p.LoX.(*syntax.Ident)
				if !ok || p.HiX != nil {
					errf("E0305", p.Pos, "patterns for enum `%s` must be variants like `%s::%s`", st.Name, st.Name, st.Variants[0].Name)
				}
				if t := c.infer(id, st); !types.Equal(t, st) {
					errf("E0305", p.Pos, "pattern has type `%s`, expected `%s`", t, st)
				}
				p.Lo, p.Hi = id.EnumVal, id.EnumVal
			case types.KBool:
				b, ok := p.LoX.(*syntax.BoolLit)
				if !ok || p.HiX != nil {
					errf("E0305", p.Pos, "patterns for `bool` must be `true`, `false` or `_`")
				}
				v := big.NewInt(0)
				if b.V {
					v = big.NewInt(1)
				}
				p.Lo, p.Hi = v, v
			default:
				p.Lo = c.patInt(p.LoX, st)
				p.Hi = p.Lo
				if p.HiX != nil {
					p.Hi = c.patInt(p.HiX, st)
					if p.Lo.Cmp(p.Hi) > 0 {
						errf("E0305", p.Pos, "empty range pattern %s..=%s", p.Lo, p.Hi)
					}
				}
			}
			cover = append(cover, iv{p.Lo, p.Hi})
		}
	}
	if !wild {
		sort.Slice(cover, func(i, j int) bool { return cover[i].lo.Cmp(cover[j].lo) < 0 })
		if st.Kind == types.KEnum {
			have := map[int64]bool{}
			for _, cv := range cover {
				have[cv.lo.Int64()] = true
			}
			for _, v := range st.Variants {
				if !have[v.Value] {
					errh("E0309", x.Position(), "add an arm for it or a `_` arm", "non-exhaustive match: variant `%s::%s` is not covered", st.Name, v.Name)
				}
			}
		} else {
			next := new(big.Int).Set(lo)
			for _, cv := range cover {
				if cv.lo.Cmp(next) > 0 {
					break
				}
				if n := new(big.Int).Add(cv.hi, big.NewInt(1)); n.Cmp(next) > 0 {
					next = n
				}
			}
			if next.Cmp(hi) <= 0 {
				errh("E0309", x.Position(), "add a `_` arm", "non-exhaustive match: value %s is not covered", next)
			}
		}
	}
	if !isExpr {
		for _, a := range arms {
			c.block(a.Body)
		}
		return types.Unit
	}
	var w *types.Type
	if want != nil && want.IsInt() {
		w = want
	}
	for _, a := range arms {
		c.infer(a.Val, w)
	}
	var rt *types.Type
	for _, a := range arms {
		if a.Val.Type().Kind != types.KUntyped {
			rt = a.Val.Type()
			break
		}
	}
	if rt == nil {
		if w != nil {
			rt = w
		} else {
			return types.Untyped
		}
	}
	for _, a := range arms {
		c.coerce(a.Val, rt)
	}
	return rt
}

func (c *checker) patInt(e syntax.Expr, st *types.Type) *big.Int {
	v := c.constInt(e)
	lo, hi := st.Range()
	if v.Cmp(lo) < 0 || v.Cmp(hi) > 0 {
		errf("E0301", e.Position(), "pattern %s does not fit in `%s`", v, st)
	}
	return v
}

// ---------------------------------------------------------------- generics

type bindings struct {
	types  map[string]*types.Type
	consts map[string]*big.Int
}

func isGenericName(f *syntax.FnDecl, name string, wantConst bool) bool {
	for _, g := range f.Generics {
		if g.Name == name && g.Const == wantConst {
			return true
		}
	}
	return false
}

func (c *checker) bindType(f *syntax.FnDecl, b *bindings, pos diag.Pos, name string, t *types.Type) {
	if old, ok := b.types[name]; ok {
		if !types.Equal(old, t) {
			errh("E0313", pos, "pass explicit generic arguments, e.g. `name::<...>(...)`",
				"conflicting types for generic parameter `%s` of `%s`: `%s` and `%s`", name, f.Name, old, t)
		}
		return
	}
	b.types[name] = t
}

func (c *checker) bindConst(f *syntax.FnDecl, b *bindings, pos diag.Pos, name string, v *big.Int) {
	if old, ok := b.consts[name]; ok {
		if old.Cmp(v) != 0 {
			errf("E0313", pos, "conflicting values for const parameter `%s` of `%s`: %s and %s", name, f.Name, old, v)
		}
		return
	}
	b.consts[name] = v
}

// unifyTE infers generic arguments by matching a declared parameter type against an argument type.
func (c *checker) unifyTE(f *syntax.FnDecl, te *syntax.TypeExpr, t *types.Type, b *bindings, pos diag.Pos) {
	switch te.Kind {
	case syntax.TNamed:
		if isGenericName(f, te.Name, false) {
			c.bindType(f, b, pos, te.Name, t)
		}
	case syntax.TArray:
		if t.Kind != types.KArray {
			return
		}
		c.unifyTE(f, te.Elem, t.Elem, b, pos)
		if id, ok := te.N.(*syntax.Ident); ok && isGenericName(f, id.Name, true) {
			c.bindConst(f, b, pos, id.Name, big.NewInt(int64(t.N)))
		}
	case syntax.TStr:
		if t.Kind != types.KStr {
			return
		}
		if id, ok := te.N.(*syntax.Ident); ok && isGenericName(f, id.Name, true) {
			c.bindConst(f, b, pos, id.Name, big.NewInt(int64(t.Cap)))
		}
	}
}

func (c *checker) constArg(ta syntax.GenericArg, pos diag.Pos) *big.Int {
	if ta.Val != nil {
		return c.constInt(ta.Val)
	}
	if ta.TE != nil && ta.TE.Kind == syntax.TNamed {
		return c.constInt(syntax.NewIdent(ta.TE.Pos, ta.TE.Name, nil))
	}
	errf("E0313", pos, "expected a constant generic argument")
	return nil
}

// instantiateFor infers the generic arguments of a call and returns the checked instance.
// It infers the call's arguments (once) and rewrites n.Name to the instance name.
func (c *checker) instantiateFor(n *syntax.Call, f *syntax.FnDecl) *syntax.FnDecl {
	if len(n.Args) != len(f.Params) {
		errf("E0005", n.Pos, "`%s` expects %d argument(s), found %d", f.Name, len(f.Params), len(n.Args))
	}
	argTys := make([]*types.Type, len(n.Args))
	for i, a := range n.Args {
		argTys[i] = c.infer(a, nil)
	}
	b := &bindings{types: map[string]*types.Type{}, consts: map[string]*big.Int{}}
	if len(n.TypeArgs) > len(f.Generics) {
		errf("E0313", n.Pos, "`%s` takes %d generic argument(s), %d given", f.Name, len(f.Generics), len(n.TypeArgs))
	}
	for i, ta := range n.TypeArgs {
		g := f.Generics[i]
		if g.Const {
			c.bindConst(f, b, n.Pos, g.Name, c.constArg(ta, n.Pos))
		} else {
			if ta.TE == nil {
				errf("E0313", n.Pos, "generic parameter `%s` expects a type", g.Name)
			}
			c.bindType(f, b, n.Pos, g.Name, c.resolve(ta.TE, false))
		}
	}
	for i, p := range f.Params {
		if !isUntyped(argTys[i]) {
			c.unifyTE(f, p.TE, argTys[i], b, n.Args[i].Position())
		}
	}
	// literals decide last: a parameter fed by any float literal defaults to f64, otherwise i32
	for i, p := range f.Params {
		if isUntyped(argTys[i]) && p.TE.Kind == syntax.TNamed && isGenericName(f, p.TE.Name, false) {
			if _, ok := b.types[p.TE.Name]; ok {
				continue
			}
			def := types.I32
			for j, q := range f.Params {
				if q.TE.Kind == syntax.TNamed && q.TE.Name == p.TE.Name && argTys[j].Kind == types.KUntypedF {
					def = types.F64
				}
			}
			b.types[p.TE.Name] = def
		}
	}
	for _, g := range f.Generics {
		if (g.Const && b.consts[g.Name] == nil) || (!g.Const && b.types[g.Name] == nil) {
			errh("E0313", n.Pos, fmt.Sprintf("write `%s::<...>(...)` to give it explicitly", f.Name),
				"cannot infer generic parameter `%s` of `%s`", g.Name, f.Name)
		}
	}
	inst := c.instance(f, b, n.Pos)
	n.Name = inst.Name
	n.TypeArgs = nil
	return inst
}

func instName(f *syntax.FnDecl, b *bindings) string {
	var parts []string
	for _, g := range f.Generics {
		if g.Const {
			parts = append(parts, b.consts[g.Name].String())
		} else {
			parts = append(parts, b.types[g.Name].String())
		}
	}
	return f.Name + "<" + strings.Join(parts, ",") + ">"
}

// instance clones and checks f under the given generic arguments (memoized).
func (c *checker) instance(f *syntax.FnDecl, b *bindings, pos diag.Pos) *syntax.FnDecl {
	name := instName(f, b)
	if inst := c.info.Fns[name]; inst != nil {
		return inst
	}
	constTy := map[string]*types.Type{}
	for _, g := range f.Generics {
		if !g.Const {
			continue
		}
		t := c.resolve(g.ConstTE, false)
		if !t.IsInt() {
			errf("E0313", g.Pos, "const generic parameter `%s` must have an integer type", g.Name)
		}
		lo, hi := t.Range()
		if v := b.consts[g.Name]; v.Cmp(lo) < 0 || v.Cmp(hi) > 0 {
			errf("E0313", pos, "const argument %s does not fit in `%s` (parameter `%s`)", v, t, g.Name)
		}
		constTy[g.Name] = t
	}
	inst := syntax.CloneFn(f)
	inst.Name = name
	c.info.Fns[name] = inst // registered before checking so recursion finds it
	c.info.Prog.Fns = append(c.info.Prog.Fns, inst)
	saved := *c
	c.typeSubst, c.constSubst, c.constTy = b.types, b.consts, constTy
	c.scopes, c.loopDepth, c.fn = nil, 0, nil
	err := diag.Try(func() {
		for i := range inst.Params {
			inst.Params[i].Ty = c.resolve(inst.Params[i].TE, false)
			if p := inst.Params[i]; p.Lo != nil && !p.Ty.IsInt() {
				errf("E0305", p.TE.Pos, "range refinement requires an integer parameter")
			}
		}
		inst.Ret = types.Unit
		if inst.RetTE != nil {
			inst.Ret = c.resolve(inst.RetTE, false)
		}
		c.checkFn(inst)
	})
	c.typeSubst, c.constSubst, c.constTy = saved.typeSubst, saved.constSubst, saved.constTy
	c.scopes, c.loopDepth, c.fn = saved.scopes, saved.loopDepth, saved.fn
	if err != nil {
		err.Msg += fmt.Sprintf(" (in instantiation `%s`)", name)
		diag.Fail(err)
	}
	return inst
}
