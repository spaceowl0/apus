// Package ir defines the structured low-level IR produced by the partial
// evaluator, together with a printer, optimizer and a reference VM.
//
// All scalar variables are 64-bit slots holding canonical values: signed
// types are sign-extended, unsigned types are zero-extended.
package ir

import (
	"fmt"
	"math"
	"strings"
)

type IntTy struct {
	Bits   int
	Signed bool
}

func (t IntTy) String() string {
	if t.Signed {
		return fmt.Sprintf("i%d", t.Bits)
	}
	return fmt.Sprintf("u%d", t.Bits)
}

func (t IntTy) MinS() int64 {
	if t.Bits >= 64 {
		return math.MinInt64
	}
	return -(int64(1) << (t.Bits - 1))
}
func (t IntTy) MaxS() int64 {
	if t.Bits >= 64 {
		return math.MaxInt64
	}
	return int64(1)<<(t.Bits-1) - 1
}
func (t IntTy) MaxU() uint64 {
	if t.Bits >= 64 {
		return math.MaxUint64
	}
	return uint64(1)<<t.Bits - 1
}

type Operand struct {
	Const bool
	C     uint64
	Var   int
}

func K(c uint64) Operand { return Operand{Const: true, C: c} }
func V(id int) Operand   { return Operand{Var: id} }

type Op int

const (
	Add Op = iota
	Sub
	Mul
	Div
	Rem
	And
	Or
	Xor
	Shl
	Shr
	Eq
	Ne
	Lt
	Le
	Gt
	Ge
	Min
	Max
)

var opNames = [...]string{"add", "sub", "mul", "div", "rem", "and", "or", "xor", "shl", "shr", "eq", "ne", "lt", "le", "gt", "ge", "min", "max"}

func (o Op) String() string { return opNames[o] }
func (o Op) IsCmp() bool    { return o >= Eq && o <= Ge }

type TrapCode int

const (
	TrapNone TrapCode = iota
	TrapOverflow
	TrapDivZero
	TrapIndex
	TrapBound
	TrapRequire
	TrapShift
	TrapInput
)

var trapNames = [...]string{"none", "arithmetic overflow", "division by zero", "index out of bounds",
	"loop bound exceeded", "require failed", "shift amount out of range", "invalid input"}

func (t TrapCode) String() string { return trapNames[t] }

type Inst interface{}

type Mov struct {
	Dst int
	A   Operand
}
type Bin struct {
	Dst   int
	Op    Op
	A, B  Operand
	Ty    IntTy // operand type
	Check bool  // may trap (overflow / div0 / shift); proven-safe ops have Check=false
}
type Neg struct {
	Dst   int
	A     Operand
	Ty    IntTy
	Check bool
}
type Not struct {
	Dst int
	A   Operand
}
type Cast struct {
	Dst      int
	A        Operand
	From, To IntTy
}
type Clamp struct {
	Dst      int
	A        Operand
	Lo, Hi   Operand
	Ty       IntTy
	TrapMode bool // true: trap when outside (require); false: saturate (clamp)
}
type Load struct {
	Dst   int
	Arr   int
	Idx   Operand
	Check bool
}
type Store struct {
	Arr   int
	Idx   Operand
	A     Operand
	Check bool
}
type ArrInit struct {
	Arr  int
	Vals []Operand
}

// FBin is a binary float operation; comparisons write 0/1.
type FBin struct {
	Dst  int
	Op   FOp
	A, B Operand
	F32  bool
}

// FUn is a unary float operation (FIsNan writes 0/1).
type FUn struct {
	Dst int
	Op  FUOp
	A   Operand
	F32 bool
}

// FCvt converts between integer and float representations.
type FCvtKind int

const (
	CvtIntToFloat   FCvtKind = iota // From (int type) -> float of width F32
	CvtFloatToInt                   // float of width F32 -> To (int type), traps when out of range or NaN
	CvtFloatToFloat                 // F32 = source width (f32->f64 when true, f64->f32 when false)
)

type FCvt struct {
	Dst  int
	A    Operand
	Kind FCvtKind
	Int  IntTy
	F32  bool
}

type WriteFloat struct {
	A   Operand
	F32 bool
}
type ReadFloat struct {
	Dst int
	F32 bool
}

// TLoad reads from a read-only constant table (placed in .rodata by the backend).
type TLoad struct {
	Dst, Tab int
	Idx      Operand
	Check    bool
}

// BufLoad reads one byte of a runtime string buffer; traps unless Idx < Len.
type BufLoad struct {
	Dst, Buf int
	Idx, Len Operand
}

// StrEq sets Dst to 1 when the runtime string equals the constant S.
type StrEq struct {
	Dst, Buf int
	Len      Operand
	S        string
}

// StrPart is one piece of a runtime-built string.
type StrPart struct {
	Kind int // 0 static text S, 1 runtime string (Buf, Len), 2 decimal integer (A, Ty), 3 float (A, F32)
	F32  bool
	S    string
	Buf  int
	Len  Operand
	A    Operand
	Ty   IntTy
}

// StrBuild concatenates Parts into buffer Buf and stores the length in Len.
type StrBuild struct {
	Buf, Len int
	Parts    []StrPart
}

// BufEq sets Dst to 1 when two runtime strings are equal.
type BufEq struct {
	Dst, BufA, BufB int
	LenA, LenB      Operand
}
type ArrFill struct {
	Arr int
	Val Operand
}
type ArrCopy struct{ Dst, Src int }
type ReadLine struct{ Buf, Len, Cap int }
type ReadInt struct {
	Dst int
	Ty  IntTy
}
type WriteStr struct{ S string }
type WriteBuf struct {
	Buf int
	Len Operand
}
type WriteDec struct {
	A  Operand
	Ty IntTy
}
type Alloc struct {
	Dst   int
	Arena int
	Size  Operand
}
type Mark struct{ Slot, Arena int }
type Reset struct{ Slot, Arena int }
type If struct {
	Cond       Operand
	Then, Else []Inst
}
type While struct {
	Pre   []Inst
	Cond  Operand
	Body  []Inst
	Bound int // maximum body executions; -1 when proven (no runtime check)
}
type Ret struct {
	A   Operand
	Has bool
}
type Trap struct{ Code TrapCode }

type ArrInfo struct {
	N  int
	Ty IntTy
}
type Arena struct {
	Name string
	Size int
}

// Table is an immutable constant array (values canonical per Ty).
type Table struct {
	Ty   IntTy
	Vals []uint64
}

type Program struct {
	NVars  int
	Tables []Table
	Arrs   []ArrInfo
	Bufs   []int
	Arenas []Arena
	Body   []Inst
	RetTy  IntTy
	HasRet bool
}

// CountInsts counts instructions recursively.
func CountInsts(b []Inst) int {
	n := 0
	for _, in := range b {
		n++
		switch s := in.(type) {
		case If:
			n += CountInsts(s.Then) + CountInsts(s.Else)
		case While:
			n += CountInsts(s.Pre) + CountInsts(s.Body)
		}
	}
	return n
}

// RodataBytes is the size of all constant tables.
func (p *Program) RodataBytes() int {
	n := 0
	for _, t := range p.Tables {
		n += len(t.Vals) * (t.Ty.Bits / 8)
	}
	return n
}

// FrameBytes estimates the stack frame of the single residual function.
func (p *Program) FrameBytes() int {
	n := p.NVars * 8
	for _, a := range p.Arrs {
		n += a.N * (a.Ty.Bits / 8)
	}
	for _, c := range p.Bufs {
		n += c
	}
	return n
}

func (o Operand) String() string {
	if o.Const {
		return fmt.Sprintf("%d", int64(o.C))
	}
	return fmt.Sprintf("v%d", o.Var)
}

// Format renders the program as readable text.
func Format(p *Program) string {
	var b strings.Builder
	fmt.Fprintf(&b, "; vars=%d arrays=%d bufs=%d frame=%dB rodata=%dB\n", p.NVars, len(p.Arrs), len(p.Bufs), p.FrameBytes(), p.RodataBytes())
	for i, t := range p.Tables {
		fmt.Fprintf(&b, "; table %d: %d x %s\n", i, len(t.Vals), t.Ty)
	}
	for i, a := range p.Arenas {
		fmt.Fprintf(&b, "; arena %d %s size=%d\n", i, a.Name, a.Size)
	}
	b.WriteString("main:\n")
	fmtBlock(&b, p.Body, 1)
	return b.String()
}

func fmtBlock(b *strings.Builder, insts []Inst, d int) {
	pad := strings.Repeat("  ", d)
	for _, in := range insts {
		switch s := in.(type) {
		case Mov:
			fmt.Fprintf(b, "%sv%d = %s\n", pad, s.Dst, s.A)
		case Bin:
			chk := ""
			if s.Check {
				chk = "!"
			}
			fmt.Fprintf(b, "%sv%d = %s%s.%s %s, %s\n", pad, s.Dst, s.Op, chk, s.Ty, s.A, s.B)
		case Neg:
			fmt.Fprintf(b, "%sv%d = neg.%s %s\n", pad, s.Dst, s.Ty, s.A)
		case Not:
			fmt.Fprintf(b, "%sv%d = not %s\n", pad, s.Dst, s.A)
		case Cast:
			fmt.Fprintf(b, "%sv%d = cast %s -> %s %s\n", pad, s.Dst, s.From, s.To, s.A)
		case Clamp:
			m := "clamp"
			if s.TrapMode {
				m = "require"
			}
			fmt.Fprintf(b, "%sv%d = %s.%s %s, %s, %s\n", pad, s.Dst, m, s.Ty, s.A, s.Lo, s.Hi)
		case Load:
			fmt.Fprintf(b, "%sv%d = load a%d[%s]%s\n", pad, s.Dst, s.Arr, s.Idx, bang(s.Check))
		case Store:
			fmt.Fprintf(b, "%sstore a%d[%s] = %s%s\n", pad, s.Arr, s.Idx, s.A, bang(s.Check))
		case ArrInit:
			var vs []string
			for _, v := range s.Vals {
				vs = append(vs, v.String())
			}
			fmt.Fprintf(b, "%sa%d = [%s]\n", pad, s.Arr, strings.Join(vs, ", "))
		case FBin:
			fmt.Fprintf(b, "%sv%d = %s%s %s, %s\n", pad, s.Dst, s.Op, fsuffix(s.F32), s.A, s.B)
		case FUn:
			fmt.Fprintf(b, "%sv%d = %s%s %s\n", pad, s.Dst, s.Op, fsuffix(s.F32), s.A)
		case FCvt:
			switch s.Kind {
			case CvtIntToFloat:
				fmt.Fprintf(b, "%sv%d = cvt %s -> f%s %s\n", pad, s.Dst, s.Int, fwidth(s.F32), s.A)
			case CvtFloatToInt:
				fmt.Fprintf(b, "%sv%d = cvt! f%s -> %s %s\n", pad, s.Dst, fwidth(s.F32), s.Int, s.A)
			default:
				fmt.Fprintf(b, "%sv%d = cvt f%s -> f%s %s\n", pad, s.Dst, fwidth(s.F32), fwidth(!s.F32), s.A)
			}
		case WriteFloat:
			fmt.Fprintf(b, "%swrite_float%s %s\n", pad, fsuffix(s.F32), s.A)
		case ReadFloat:
			fmt.Fprintf(b, "%sv%d = read_float%s\n", pad, s.Dst, fsuffix(s.F32))
		case StrBuild:
			var ps []string
			for _, q := range s.Parts {
				switch q.Kind {
				case 0:
					ps = append(ps, fmt.Sprintf("%q", q.S))
				case 1:
					ps = append(ps, fmt.Sprintf("b%d[:%s]", q.Buf, q.Len))
				case 3:
					ps = append(ps, fmt.Sprintf("float(%s)", q.A))
				default:
					ps = append(ps, fmt.Sprintf("dec.%s(%s)", q.Ty, q.A))
				}
			}
			fmt.Fprintf(b, "%sb%d, v%d = build %s\n", pad, s.Buf, s.Len, strings.Join(ps, " ++ "))
		case BufEq:
			fmt.Fprintf(b, "%sv%d = b%d[:%s] == b%d[:%s]\n", pad, s.Dst, s.BufA, s.LenA, s.BufB, s.LenB)
		case TLoad:
			fmt.Fprintf(b, "%sv%d = tload t%d[%s]%s\n", pad, s.Dst, s.Tab, s.Idx, bang(s.Check))
		case BufLoad:
			fmt.Fprintf(b, "%sv%d = bufload b%d[%s] len %s\n", pad, s.Dst, s.Buf, s.Idx, s.Len)
		case StrEq:
			fmt.Fprintf(b, "%sv%d = streq b%d[:%s] == %q\n", pad, s.Dst, s.Buf, s.Len, s.S)
		case ArrFill:
			fmt.Fprintf(b, "%sa%d = fill %s\n", pad, s.Arr, s.Val)
		case ArrCopy:
			fmt.Fprintf(b, "%sa%d = copy a%d\n", pad, s.Dst, s.Src)
		case ReadLine:
			fmt.Fprintf(b, "%sb%d, v%d = read_line cap=%d\n", pad, s.Buf, s.Len, s.Cap)
		case ReadInt:
			fmt.Fprintf(b, "%sv%d = read_int.%s\n", pad, s.Dst, s.Ty)
		case WriteStr:
			fmt.Fprintf(b, "%swrite %q  ; %dB\n", pad, s.S, len(s.S))
		case WriteBuf:
			fmt.Fprintf(b, "%swrite b%d[:%s]\n", pad, s.Buf, s.Len)
		case WriteDec:
			fmt.Fprintf(b, "%swrite_dec.%s %s\n", pad, s.Ty, s.A)
		case Alloc:
			fmt.Fprintf(b, "%sv%d = alloc arena%d, %s\n", pad, s.Dst, s.Arena, s.Size)
		case Mark:
			fmt.Fprintf(b, "%sv%d = mark arena%d\n", pad, s.Slot, s.Arena)
		case Reset:
			fmt.Fprintf(b, "%sreset arena%d to v%d\n", pad, s.Arena, s.Slot)
		case If:
			fmt.Fprintf(b, "%sif %s {\n", pad, s.Cond)
			fmtBlock(b, s.Then, d+1)
			if len(s.Else) > 0 {
				fmt.Fprintf(b, "%s} else {\n", pad)
				fmtBlock(b, s.Else, d+1)
			}
			fmt.Fprintf(b, "%s}\n", pad)
		case While:
			fmt.Fprintf(b, "%sloop (bound %d) {\n", pad, s.Bound)
			fmtBlock(b, s.Pre, d+1)
			fmt.Fprintf(b, "%s  break unless %s\n", pad, s.Cond)
			fmtBlock(b, s.Body, d+1)
			fmt.Fprintf(b, "%s}\n", pad)
		case Ret:
			if s.Has {
				fmt.Fprintf(b, "%sret %s\n", pad, s.A)
			} else {
				fmt.Fprintf(b, "%sret\n", pad)
			}
		case Trap:
			fmt.Fprintf(b, "%strap %s\n", pad, s.Code)
		default:
			fmt.Fprintf(b, "%s?? %T\n", pad, in)
		}
	}
}

func fwidth(f32 bool) string {
	if f32 {
		return "32"
	}
	return "64"
}
func fsuffix(f32 bool) string { return ".f" + fwidth(f32) }

func bang(c bool) string {
	if c {
		return " !"
	}
	return ""
}
