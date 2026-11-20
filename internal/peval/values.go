// Package peval implements Apus's online partial evaluator: it executes the
// typed AST symbolically, folding everything that is statically known and
// emitting a residual IR for the rest.
package peval

import (
	"fmt"
	"math/big"
	"strings"

	"apus/internal/ir"
	"apus/internal/types"
)

type vk int

const (
	kConst  vk = iota // statically known scalar or string
	kDyn              // runtime scalar (IR var) or runtime string (buf + length var)
	kRope             // string of static runs and dynamic holes
	kArr              // array with individually tracked elements
	kStruct           // struct with individually tracked fields
	kMem              // array living in IR memory
	kSoA              // array of flat structs held as one kMem column per field (elems = columns)
	kUnit
)

type part struct {
	s string
	d *V // non-nil: dynamic hole (kDyn string or integer)
}

// V is an abstract value. Scalar V's are immutable; aggregates are mutated in place
// and therefore always copied by own().
type V struct {
	k       vk
	ty      *types.Type
	c       *big.Int
	f       float64 // kConst float: the value, already rounded to the type's width
	s       string
	v       int // kDyn: IR variable (for strings: the length variable)
	lo, hi  *big.Int
	excl    *big.Int // kDyn scalar is known to differ from this value (set by `x != c` narrowing)
	slot    bool     // kDyn scalar is a mutable slot (assignment emits Mov)
	buf     int      // kDyn string: buffer id
	parts   []part
	elems   []*V
	mem     int      // kMem: IR array id
	mat     *V       // kRope: cached runtime-buffer form
	capDecl int      // string: declared capacity of the variable that holds it (0 = unknown)
	matReg  int      // emission region in which mat was built
	elo     *big.Int // kMem: hull of stored element values
	ehi     *big.Int
}

var unitV = &V{k: kUnit, ty: types.Unit}

func cInt(t *types.Type, c *big.Int) *V { return &V{k: kConst, ty: t, c: c} }

// cFloat makes a constant float, rounding to the type's width. c is a zero placeholder so
// shared interval code never sees a nil range.
func cFloat(t *types.Type, f float64) *V {
	if t.Bits == 32 {
		f = float64(float32(f))
	}
	return &V{k: kConst, ty: t, c: big.NewInt(0), f: f}
}

// vbits is the canonical IR bit pattern of a constant scalar.
func vbits(v *V) uint64 {
	if v.ty.Kind == types.KFloat {
		return ir.FromFloat64(v.f, v.ty.Bits == 32)
	}
	return toBits(v.c, v.ty)
}

func cBool(b bool) *V {
	n := int64(0)
	if b {
		n = 1
	}
	return &V{k: kConst, ty: types.Bool, c: big.NewInt(n)}
}
func cStr(s string) *V { return &V{k: kConst, ty: types.Str(len(s)), s: s} }

func fixTy(t *types.Type) *types.Type {
	if t == nil || t.Kind == types.KUntyped {
		return types.I32
	}
	if t.Kind == types.KUntypedF {
		return types.F64
	}
	return t
}

func irTy(t *types.Type) ir.IntTy {
	t = fixTy(t)
	if t.Kind == types.KBool {
		return ir.IntTy{Bits: 8}
	}
	if t.Kind == types.KFloat {
		return ir.IntTy{Bits: t.Bits} // float bit patterns are zero-extended
	}
	return ir.IntTy{Bits: t.Bits, Signed: t.Signed}
}

func toBits(c *big.Int, t *types.Type) uint64 {
	if c.Sign() < 0 {
		return uint64(c.Int64())
	}
	return c.Uint64()
}

func opnd(v *V) ir.Operand {
	if v.k == kConst {
		return ir.K(vbits(v))
	}
	return ir.V(v.v)
}

// rng returns the value interval of a scalar value.
func rng(v *V) (*big.Int, *big.Int) {
	if v.k == kConst {
		return v.c, v.c
	}
	return v.lo, v.hi
}

func inRange(x *big.Int, t *types.Type) bool {
	lo, hi := t.Range()
	return x.Cmp(lo) >= 0 && x.Cmp(hi) <= 0
}

func bmin(a, b *big.Int) *big.Int {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}
func bmax(a, b *big.Int) *big.Int {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}
func babs(a *big.Int) *big.Int { return new(big.Int).Abs(a) }

func allConst(v *V) bool {
	switch v.k {
	case kConst, kUnit:
		return true
	case kArr, kStruct:
		for _, e := range v.elems {
			if !allConst(e) {
				return false
			}
		}
		return true
	}
	return false
}

// constKey serialises a fully-constant value (for memoization and table caching).
func constKey(v *V) string {
	switch v.k {
	case kConst:
		if v.ty.Kind == types.KStr {
			return fmt.Sprintf("s%q", v.s)
		}
		if v.ty.Kind == types.KFloat {
			return fmt.Sprintf("f%d", vbits(v))
		}
		return v.c.String()
	case kArr, kStruct:
		var p []string
		for _, e := range v.elems {
			p = append(p, constKey(e))
		}
		return "[" + strings.Join(p, ",") + "]"
	}
	return "?"
}

func dynScalar(t *types.Type, id int, lo, hi *big.Int) *V {
	tlo, thi := t.Range()
	if lo == nil || lo.Cmp(tlo) < 0 {
		lo = tlo
	}
	if hi == nil || hi.Cmp(thi) > 0 {
		hi = thi
	}
	if lo.Cmp(hi) > 0 { // empty interval: the producing operation always traps
		lo, hi = tlo, thi
	}
	return &V{k: kDyn, ty: t, v: id, lo: lo, hi: hi}
}

// ropeCap is the maximum byte length of a string-like value.
func strCap(v *V) int {
	switch v.k {
	case kConst:
		return len(v.s)
	case kDyn:
		return v.ty.Cap
	case kRope:
		n := 0
		for _, p := range v.parts {
			if p.d == nil {
				n += len(p.s)
			} else if p.d.ty.Kind == types.KStr {
				n += p.d.ty.Cap
			} else {
				n += p.d.ty.DecimalDigits()
			}
		}
		return n
	}
	return 0
}
