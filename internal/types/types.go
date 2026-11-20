// Package types defines the Apus type system.
package types

import (
	"fmt"
	"math/big"
	"strings"
)

type Kind int

const (
	KUnit Kind = iota
	KBool
	KInt
	KUntyped // integer literal whose type is not yet fixed
	KStr
	KArray
	KStruct
	KPtr
	KEnum
	KFloat
	KUntypedF // floating literal whose type is not yet fixed
)

type Field struct {
	Name string
	Ty   *Type
}

type Variant struct {
	Name  string
	Value int64
}

type Type struct {
	Kind     Kind
	Variants []Variant // KEnum (underlying type is u32)
	Bits     int       // KInt, KEnum
	Signed   bool      // KInt
	Cap      int       // KStr: maximum byte length
	Elem     *Type
	N        int // KArray length
	Name     string
	Fields   []Field
}

var (
	Unit     = &Type{Kind: KUnit, Name: "()"}
	Bool     = &Type{Kind: KBool, Name: "bool"}
	Untyped  = &Type{Kind: KUntyped, Name: "{integer}"}
	UntypedF = &Type{Kind: KUntypedF, Name: "{float}"}
	F32      = &Type{Kind: KFloat, Bits: 32, Signed: true, Name: "f32"}
	F64      = &Type{Kind: KFloat, Bits: 64, Signed: true, Name: "f64"}
	Ptr      = &Type{Kind: KPtr, Name: "ptr"}
)

var ints = buildInts()

func buildInts() map[string]*Type {
	m := map[string]*Type{}
	for _, b := range []int{8, 16, 32, 64} {
		m[fmt.Sprintf("u%d", b)] = &Type{Kind: KInt, Bits: b, Name: fmt.Sprintf("u%d", b)}
		m[fmt.Sprintf("i%d", b)] = &Type{Kind: KInt, Bits: b, Signed: true, Name: fmt.Sprintf("i%d", b)}
	}
	m["usize"] = m["u64"]
	m["isize"] = m["i64"]
	return m
}

// Int returns a primitive integer type by name, or nil.
func Int(name string) *Type { return ints[name] }

// Float returns f32 or f64 by name, or nil.
func Float(name string) *Type {
	switch name {
	case "f32":
		return F32
	case "f64":
		return F64
	}
	return nil
}

func (t *Type) IsFloat() bool { return t.Kind == KFloat }

var (
	U8  = ints["u8"]
	U32 = ints["u32"]
	U64 = ints["u64"]
	I32 = ints["i32"]
	I64 = ints["i64"]
)

func Str(cap int) *Type {
	return &Type{Kind: KStr, Cap: cap, Name: fmt.Sprintf("Str<%d>", cap)}
}

func Array(elem *Type, n int) *Type {
	return &Type{Kind: KArray, Elem: elem, N: n, Name: fmt.Sprintf("[%s; %d]", elem, n)}
}

func (t *Type) String() string { return t.Name }
func (t *Type) IsInt() bool    { return t.Kind == KInt }

// IsScalar covers everything that lives in a single IR variable. Floats carry
// a vacuous [0,0] "range" so shared interval bookkeeping stays harmless.
func (t *Type) IsScalar() bool {
	return t.Kind == KInt || t.Kind == KBool || t.Kind == KEnum || t.Kind == KFloat
}

func Equal(a, b *Type) bool {
	if a == b {
		return true
	}
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case KFloat:
		return a.Bits == b.Bits
	case KInt:
		return a.Bits == b.Bits && a.Signed == b.Signed
	case KStr:
		return a.Cap == b.Cap
	case KArray:
		return a.N == b.N && Equal(a.Elem, b.Elem)
	case KStruct, KEnum:
		return a.Name == b.Name
	}
	return true
}

// Assignable reports whether a value of type from can initialise a place of type to.
func Assignable(from, to *Type) bool {
	if from.Kind == KUntyped && to.Kind == KInt {
		return true
	}
	if from.Kind == KStr && to.Kind == KStr {
		return from.Cap <= to.Cap
	}
	return Equal(from, to)
}

// Range returns the inclusive value range of a scalar type.
func (t *Type) Range() (lo, hi *big.Int) {
	switch t.Kind {
	case KBool:
		return big.NewInt(0), big.NewInt(1)
	case KFloat:
		return big.NewInt(0), big.NewInt(0)
	case KEnum:
		lo, hi := t.Variants[0].Value, t.Variants[0].Value
		for _, v := range t.Variants {
			if v.Value < lo {
				lo = v.Value
			}
			if v.Value > hi {
				hi = v.Value
			}
		}
		return big.NewInt(lo), big.NewInt(hi)
	case KInt:
		one := big.NewInt(1)
		if t.Signed {
			hi = new(big.Int).Lsh(one, uint(t.Bits-1))
			lo = new(big.Int).Neg(hi)
			hi.Sub(hi, one)
			return
		}
		hi = new(big.Int).Lsh(one, uint(t.Bits))
		hi.Sub(hi, one)
		return big.NewInt(0), hi
	}
	return nil, nil
}

func (t *Type) Align() int {
	switch t.Kind {
	case KBool:
		return 1
	case KEnum:
		return 4
	case KInt, KFloat:
		return t.Bits / 8
	case KStr:
		return 4
	case KPtr:
		return 8
	case KArray:
		return t.Elem.Align()
	case KStruct:
		a := 1
		for _, f := range t.Fields {
			if fa := f.Ty.Align(); fa > a {
				a = fa
			}
		}
		return a
	}
	return 1
}

// Size is the C-like layout size in bytes. Str<N> is a u32 length plus N bytes.
func (t *Type) Size() int {
	switch t.Kind {
	case KBool:
		return 1
	case KEnum:
		return 4
	case KInt, KFloat:
		return t.Bits / 8
	case KStr:
		return 4 + t.Cap
	case KPtr:
		return 8
	case KArray:
		return t.N * t.Elem.Size()
	case KStruct:
		off := 0
		for _, f := range t.Fields {
			a := f.Ty.Align()
			off = (off + a - 1) / a * a
			off += f.Ty.Size()
		}
		a := t.Align()
		return (off + a - 1) / a * a
	}
	return 0
}

// Offset is the byte offset of field i under C layout rules.
func (t *Type) Offset(i int) int {
	off := 0
	for k, f := range t.Fields {
		a := f.Ty.Align()
		off = (off + a - 1) / a * a
		if k == i {
			return off
		}
		off += f.Ty.Size()
	}
	return off
}

func (t *Type) VariantIndex(name string) int {
	for i, v := range t.Variants {
		if v.Name == name {
			return i
		}
	}
	return -1
}

func (t *Type) FieldIndex(name string) int {
	for i, f := range t.Fields {
		if f.Name == name {
			return i
		}
	}
	return -1
}

// DecimalDigits is the maximum number of characters needed to print a value of t.
func (t *Type) DecimalDigits() int {
	if t.Kind == KFloat {
		return 24 // "-9000000000.000000", "-inf", "nan", "-big"
	}
	lo, hi := t.Range()
	n := len(hi.String())
	if l := len(lo.String()); l > n {
		n = l
	}
	return n
}

func (t *Type) Describe() string {
	if t.Kind == KStruct {
		var parts []string
		for _, f := range t.Fields {
			parts = append(parts, f.Name+": "+f.Ty.String())
		}
		return t.Name + " { " + strings.Join(parts, ", ") + " }"
	}
	return t.String()
}
