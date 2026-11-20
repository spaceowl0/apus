package syntax

import (
	"math/big"

	"apus/internal/diag"
	"apus/internal/types"
)

// Constructors for synthetic, already-typed nodes (used when lowering match).

func NewIntLit(pos diag.Pos, v *big.Int, t *types.Type) *IntLit {
	return &IntLit{base: base{Pos: pos, Ty: t}, V: v}
}

func NewIdent(pos diag.Pos, name string, t *types.Type) *Ident {
	return &Ident{base: base{Pos: pos, Ty: t}, Name: name}
}

func NewBinary(pos diag.Pos, op string, l, r Expr, t *types.Type) *Binary {
	return &Binary{base: base{Pos: pos, Ty: t}, Op: op, L: l, R: r}
}
