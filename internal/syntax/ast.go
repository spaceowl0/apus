// Package syntax contains the Apus lexer, AST and parser.
package syntax

import (
	"math/big"

	"apus/internal/diag"
	"apus/internal/types"
)

// ---- expressions

type Expr interface {
	Position() diag.Pos
	Type() *types.Type
	SetType(*types.Type)
}

type base struct {
	Pos diag.Pos
	Ty  *types.Type
}

func (b *base) Position() diag.Pos    { return b.Pos }
func (b *base) Type() *types.Type     { return b.Ty }
func (b *base) SetType(t *types.Type) { b.Ty = t }

type IntLit struct {
	base
	V *big.Int
}
type FloatLit struct {
	base
	Text string
}
type StrLit struct {
	base
	V string
}
type BoolLit struct {
	base
	V bool
}
type Ident struct {
	base
	Name    string
	EnumVal *big.Int // set by sema when the name is an enum variant (`Color::Red`)
}
type Unary struct {
	base
	Op string
	X  Expr
}
type Binary struct {
	base
	Op   string
	L, R Expr
}

// GenericArg is one explicit generic argument: a type or a constant value.
type GenericArg struct {
	TE  *TypeExpr
	Val Expr
}

type Call struct {
	base
	Name     string
	Args     []Expr
	TypeArgs []GenericArg // f::<A, B>(...)
	Embed    []byte       // contents read by sema for embed("path")
}
type Cast struct {
	base
	X  Expr
	To *TypeExpr
}
type Index struct {
	base
	X, I Expr
}
type Field struct {
	base
	X     Expr
	Name  string
	Index int // resolved by sema
}
type FieldInit struct {
	Pos  diag.Pos
	Name string
	X    Expr
}
type StructLit struct {
	base
	Name   string
	Fields []FieldInit
}
type ArrayLit struct {
	base
	Elems []Expr
}
type ArrayRep struct {
	base
	Val Expr
	N   Expr
	Len int // resolved by sema
}
type Static struct {
	base
	X Expr
}

// Pattern is one alternative of a match arm: `_`, a value, or a range `lo..=hi`.
type Pattern struct {
	Pos      diag.Pos
	Wild     bool
	LoX, HiX Expr     // as written (HiX is nil unless a range)
	Lo, Hi   *big.Int // resolved by sema (inclusive)
}

type Arm struct {
	Pos  diag.Pos
	Pats []*Pattern
	Body []Stmt // statement form
	Val  Expr   // expression form
}

type MatchExpr struct {
	base
	X    Expr
	Arms []*Arm
}

// ---- statements

type Stmt interface{ Position() diag.Pos }

type Let struct {
	Pos  diag.Pos
	Name string
	Mut  bool
	TE   *TypeExpr
	Init Expr
	Ty   *types.Type
}
type Assign struct {
	Pos    diag.Pos
	Target Expr
	X      Expr
}
type ExprStmt struct {
	Pos diag.Pos
	X   Expr
}
type If struct {
	Pos        diag.Pos
	Cond       Expr
	Then, Else []Stmt
}
type While struct {
	Pos   diag.Pos
	Cond  Expr
	Bound int
	Body  []Stmt
}
type For struct {
	Pos    diag.Pos
	Var    string
	Lo, Hi Expr
	Body   []Stmt
	Ty     *types.Type
}
type Return struct {
	Pos diag.Pos
	X   Expr
}
type Block struct {
	Pos  diag.Pos
	Body []Stmt
}
type Region struct {
	Pos   diag.Pos
	Arena string
	Body  []Stmt
}
type Break struct{ Pos diag.Pos }
type Continue struct{ Pos diag.Pos }
type MatchStmt struct {
	Pos  diag.Pos
	X    Expr
	Arms []*Arm
}

func (s *Let) Position() diag.Pos       { return s.Pos }
func (s *Assign) Position() diag.Pos    { return s.Pos }
func (s *ExprStmt) Position() diag.Pos  { return s.Pos }
func (s *If) Position() diag.Pos        { return s.Pos }
func (s *While) Position() diag.Pos     { return s.Pos }
func (s *For) Position() diag.Pos       { return s.Pos }
func (s *Return) Position() diag.Pos    { return s.Pos }
func (s *Region) Position() diag.Pos    { return s.Pos }
func (s *Block) Position() diag.Pos     { return s.Pos }
func (s *Break) Position() diag.Pos     { return s.Pos }
func (s *Continue) Position() diag.Pos  { return s.Pos }
func (s *MatchStmt) Position() diag.Pos { return s.Pos }

// ---- items

type TypeKind int

const (
	TNamed TypeKind = iota
	TStr
	TArray
)

type TypeExpr struct {
	Pos  diag.Pos
	Kind TypeKind
	Name string
	N    Expr      // Str<N> capacity or array length
	Elem *TypeExpr // arrays
}

type ConstDecl struct {
	Pos  diag.Pos
	Name string
	TE   *TypeExpr
	X    Expr
	Ty   *types.Type
}
type FieldDecl struct {
	Name string
	TE   *TypeExpr
}
type StructDecl struct {
	Pos    diag.Pos
	Name   string
	Fields []FieldDecl
}
type VariantDecl struct {
	Name string
	Val  *big.Int // nil: previous + 1
}
type EnumDecl struct {
	Pos      diag.Pos
	Name     string
	Variants []VariantDecl
}
type ArenaDecl struct {
	Pos  diag.Pos
	Name string
	Size int
}
type Param struct {
	Name   string
	TE     *TypeExpr
	Lo, Hi *big.Int // optional refinement `in lo..=hi`
	Ty     *types.Type
}

// Generic is a type parameter (`T`) or a constant parameter (`const N: usize`).
type Generic struct {
	Pos     diag.Pos
	Name    string
	Const   bool
	ConstTE *TypeExpr
}

type FnDecl struct {
	Generics  []Generic
	Pos       diag.Pos
	Name      string
	Pure, IO  bool
	Params    []Param
	RetTE     *TypeExpr
	Ret       *types.Type
	BoundRec  int // -1 when absent
	Body      []Stmt
	NestedRet bool // has a return that is not a top-level statement
}

type Program struct {
	Enums   []*EnumDecl
	Consts  []*ConstDecl
	Structs []*StructDecl
	Arenas  []*ArenaDecl
	Fns     []*FnDecl
}
