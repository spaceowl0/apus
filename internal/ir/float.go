package ir

import (
	"bufio"
	"math"
	"strconv"
)

// Floating-point values live in the 64-bit slots as IEEE bit patterns: f64 uses
// all 64 bits, f32 the low 32 bits (zero-extended). All semantics used by the VM
// and by compile-time folding are defined here; the LLVM backend and C runtime
// implement exactly the same operations.

func F64Of(bits uint64) float64 { return math.Float64frombits(bits) }
func F32Of(bits uint64) float32 { return math.Float32frombits(uint32(bits)) }
func BitsOf64(f float64) uint64 { return math.Float64bits(f) }
func BitsOf32(f float32) uint64 { return uint64(math.Float32bits(f)) }

// ToFloat64 widens a canonical float to float64 (exact for f32).
func ToFloat64(bits uint64, f32 bool) float64 {
	if f32 {
		return float64(F32Of(bits))
	}
	return F64Of(bits)
}

// FromFloat64 rounds f to the canonical representation of the given width.
func FromFloat64(f float64, f32 bool) uint64 {
	if f32 {
		return BitsOf32(float32(f))
	}
	return BitsOf64(f)
}

type FOp int

const (
	FAdd FOp = iota
	FSub
	FMul
	FDiv
	FMin
	FMax
	FEq
	FNe
	FLt
	FLe
	FGt
	FGe
)

var fopNames = [...]string{"fadd", "fsub", "fmul", "fdiv", "fmin", "fmax", "feq", "fne", "flt", "fle", "fgt", "fge"}

func (o FOp) String() string { return fopNames[o] }
func (o FOp) IsCmp() bool    { return o >= FEq }

type FUOp int

const (
	FNeg FUOp = iota
	FSqrt
	FFloor
	FCeil
	FTrunc
	FAbs
	FIsNan
)

var fuNames = [...]string{"fneg", "fsqrt", "ffloor", "fceil", "ftrunc", "fabs", "fisnan"}

func (o FUOp) String() string { return fuNames[o] }

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// EvalFBin performs one IEEE operation at the given width. Comparisons return 1/0
// with ordered semantics (everything but `!=` is false when an operand is NaN).
func EvalFBin(op FOp, a, b uint64, f32 bool) uint64 {
	if f32 {
		x, y := F32Of(a), F32Of(b)
		switch op {
		case FAdd:
			return BitsOf32(x + y)
		case FSub:
			return BitsOf32(x - y)
		case FMul:
			return BitsOf32(x * y)
		case FDiv:
			return BitsOf32(x / y)
		case FMin:
			if y < x {
				return BitsOf32(y)
			}
			return BitsOf32(x)
		case FMax:
			if y > x {
				return BitsOf32(y)
			}
			return BitsOf32(x)
		case FEq:
			return b2u(x == y)
		case FNe:
			return b2u(x != y)
		case FLt:
			return b2u(x < y)
		case FLe:
			return b2u(x <= y)
		case FGt:
			return b2u(x > y)
		}
		return b2u(x >= y)
	}
	x, y := F64Of(a), F64Of(b)
	switch op {
	case FAdd:
		return BitsOf64(x + y)
	case FSub:
		return BitsOf64(x - y)
	case FMul:
		return BitsOf64(x * y)
	case FDiv:
		return BitsOf64(x / y)
	case FMin:
		if y < x {
			return BitsOf64(y)
		}
		return BitsOf64(x)
	case FMax:
		if y > x {
			return BitsOf64(y)
		}
		return BitsOf64(x)
	case FEq:
		return b2u(x == y)
	case FNe:
		return b2u(x != y)
	case FLt:
		return b2u(x < y)
	case FLe:
		return b2u(x <= y)
	case FGt:
		return b2u(x > y)
	}
	return b2u(x >= y)
}

// EvalFUn applies a unary float operation.
func EvalFUn(op FUOp, a uint64, f32 bool) uint64 {
	x := ToFloat64(a, f32)
	switch op {
	case FNeg:
		if f32 {
			return BitsOf32(-F32Of(a))
		}
		return BitsOf64(-x)
	case FSqrt:
		return FromFloat64(math.Sqrt(x), f32) // f64 sqrt rounded to f32 is correctly rounded
	case FFloor:
		return FromFloat64(math.Floor(x), f32)
	case FCeil:
		return FromFloat64(math.Ceil(x), f32)
	case FTrunc:
		return FromFloat64(math.Trunc(x), f32)
	case FAbs:
		return FromFloat64(math.Abs(x), f32)
	}
	return b2u(x != x)
}

// FloatToInt converts toward zero; NaN and out-of-range values fail (ok=false).
func FloatToInt(a uint64, f32 bool, to IntTy) (uint64, bool) {
	t := math.Trunc(ToFloat64(a, f32))
	if t != t {
		return 0, false
	}
	hi := math.Ldexp(1, to.Bits) // exclusive upper bound for unsigned
	lo := 0.0
	if to.Signed {
		hi = math.Ldexp(1, to.Bits-1)
		lo = -hi
	}
	if !(t >= lo && t < hi) {
		return 0, false
	}
	if to.Signed {
		return uint64(int64(t)), true
	}
	return uint64(t), true
}

// IntToFloat converts with round-to-nearest-even.
func IntToFloat(a uint64, from IntTy, f32 bool) uint64 {
	if from.Signed {
		if f32 {
			return BitsOf32(float32(int64(a)))
		}
		return BitsOf64(float64(int64(a)))
	}
	if f32 {
		return BitsOf32(float32(a))
	}
	return BitsOf64(float64(a))
}

// FloatToFloat converts between widths: from f32 to f64 when srcF32, else f64 to f32.
func FloatToFloat(a uint64, srcF32 bool) uint64 {
	if srcF32 {
		return BitsOf64(float64(F32Of(a)))
	}
	return BitsOf32(float32(F64Of(a)))
}

// FormatFloat is the text form used by str(float): six fractional digits, computed as
// round-half-even(|v| * 1e6) split into integer and fractional parts. NaN is "nan", infinities
// "inf"/"-inf", and magnitudes of 9e9 and above print as "big"/"-big".
func FormatFloat(v float64) string {
	switch {
	case v != v:
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	}
	a := math.Abs(v)
	y := a * 1e6
	if !(y < 9007199254740992.0) {
		if v < 0 {
			return "-big"
		}
		return "big"
	}
	n := uint64(math.RoundToEven(y))
	s := ""
	if v < 0 && n != 0 {
		s = "-"
	}
	frac := strconv.FormatUint(n%1000000, 10)
	for len(frac) < 6 {
		frac = "0" + frac
	}
	return s + strconv.FormatUint(n/1000000, 10) + "." + frac
}

var pow10 = [...]float64{1, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10, 1e11, 1e12, 1e13, 1e14, 1e15}

// ReadFloatFrom parses `-?digits(.digits)?` with at most 15 digits in total. The value is
// (all digits as an integer) / 10^(fraction digits): one correctly rounded IEEE division of two
// exact operands, so the VM and the C runtime agree bit for bit.
func ReadFloatFrom(in *bufio.Reader, f32 bool) (uint64, bool) {
	var c byte
	var err error
	for {
		c, err = in.ReadByte()
		if err != nil {
			return 0, false
		}
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			break
		}
	}
	neg := false
	if c == '-' {
		neg = true
		if c, err = in.ReadByte(); err != nil {
			return 0, false
		}
	}
	if c < '0' || c > '9' {
		return 0, false
	}
	var n uint64
	digits, frac := 0, 0
	seenDot := false
	for {
		if c >= '0' && c <= '9' {
			digits++
			if digits > 15 {
				return 0, false
			}
			n = n*10 + uint64(c-'0')
			if seenDot {
				frac++
			}
		} else if c == '.' && !seenDot {
			seenDot = true
			if c, err = in.ReadByte(); err != nil || c < '0' || c > '9' {
				return 0, false
			}
			continue
		} else {
			in.UnreadByte()
			break
		}
		if c, err = in.ReadByte(); err != nil {
			break
		}
	}
	v := float64(n) / pow10[frac]
	if neg {
		v = -v
	}
	return FromFloat64(v, f32), true
}
