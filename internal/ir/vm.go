package ir

import (
	"bufio"
	"io"
	"math/bits"
	"strconv"
)

// Result is the outcome of executing a program in the VM.
type Result struct {
	Ret   uint64
	Has   bool
	Trap  TrapCode
	Steps int64
}

// ExitCode maps a result to a process exit status (101 for traps, like Rust panics).
func (r Result) ExitCode(p *Program) int {
	if r.Trap != TrapNone {
		return 101
	}
	if !r.Has {
		return 0
	}
	return int(r.Ret & 0xff)
}

type vm struct {
	p     *Program
	vars  []uint64
	arrs  [][]uint64
	bufs  [][]byte
	tops  []uint64
	in    *bufio.Reader
	out   *bufio.Writer
	steps int64
	trap  TrapCode
	ret   uint64
	has   bool
	done  bool
}

// Run executes p reading from in and writing to out.
func Run(p *Program, in io.Reader, out io.Writer) Result {
	m := &vm{p: p, vars: make([]uint64, p.NVars+1), in: bufio.NewReader(in), out: bufio.NewWriter(out)}
	for _, a := range p.Arrs {
		m.arrs = append(m.arrs, make([]uint64, a.N))
	}
	for _, c := range p.Bufs {
		m.bufs = append(m.bufs, make([]byte, c))
	}
	m.tops = make([]uint64, len(p.Arenas))
	m.exec(p.Body)
	m.out.Flush()
	return Result{Ret: m.ret, Has: m.has, Trap: m.trap, Steps: m.steps}
}

func (m *vm) val(o Operand) uint64 {
	if o.Const {
		return o.C
	}
	return m.vars[o.Var]
}

func (m *vm) fail(c TrapCode) bool {
	m.trap = c
	m.done = true
	return true
}

func canon(v uint64, t IntTy) uint64 {
	if t.Bits >= 64 {
		return v
	}
	if t.Signed {
		sh := uint(64 - t.Bits)
		return uint64(int64(v<<sh) >> sh)
	}
	return v & t.MaxU()
}

func fitsS(r int64, t IntTy) bool { return r >= t.MinS() && r <= t.MaxS() }

// EvalBin performs a checked binary operation exactly as the generated code must.
func EvalBin(op Op, a, b uint64, t IntTy) (uint64, TrapCode) {
	if op.IsCmp() {
		var res bool
		if t.Signed {
			x, y := int64(a), int64(b)
			switch op {
			case Eq:
				res = x == y
			case Ne:
				res = x != y
			case Lt:
				res = x < y
			case Le:
				res = x <= y
			case Gt:
				res = x > y
			case Ge:
				res = x >= y
			}
		} else {
			switch op {
			case Eq:
				res = a == b
			case Ne:
				res = a != b
			case Lt:
				res = a < b
			case Le:
				res = a <= b
			case Gt:
				res = a > b
			case Ge:
				res = a >= b
			}
		}
		if res {
			return 1, 0
		}
		return 0, 0
	}
	switch op {
	case Min, Max:
		less := a < b
		if t.Signed {
			less = int64(a) < int64(b)
		}
		if (op == Min) == less {
			return a, 0
		}
		return b, 0
	case And:
		return a & b, 0
	case Or:
		return a | b, 0
	case Xor:
		return a ^ b, 0
	case Shl, Shr:
		if b >= uint64(t.Bits) {
			return 0, TrapShift
		}
		if op == Shl {
			return canon(a<<b, t), 0
		}
		if t.Signed {
			return uint64(int64(a) >> b), 0
		}
		return a >> b, 0
	}
	if !t.Signed {
		var r uint64
		switch op {
		case Add:
			var c uint64
			r, c = bits.Add64(a, b, 0)
			if c != 0 || r > t.MaxU() {
				return 0, TrapOverflow
			}
		case Sub:
			if a < b {
				return 0, TrapOverflow
			}
			r = a - b
		case Mul:
			hi, lo := bits.Mul64(a, b)
			if hi != 0 || lo > t.MaxU() {
				return 0, TrapOverflow
			}
			r = lo
		case Div:
			if b == 0 {
				return 0, TrapDivZero
			}
			r = a / b
		case Rem:
			if b == 0 {
				return 0, TrapDivZero
			}
			r = a % b
		}
		return r, 0
	}
	x, y := int64(a), int64(b)
	var r int64
	switch op {
	case Add:
		r = x + y
		if ((x >= 0) == (y >= 0) && (r >= 0) != (x >= 0)) || !fitsS(r, t) {
			return 0, TrapOverflow
		}
	case Sub:
		r = x - y
		if ((x >= 0) != (y >= 0) && (r >= 0) != (x >= 0)) || !fitsS(r, t) {
			return 0, TrapOverflow
		}
	case Mul:
		if x == 0 || y == 0 {
			return 0, 0
		}
		r = x * y
		if r/y != x || (x == -1 && y == -1<<63) || (y == -1 && x == -1<<63) || !fitsS(r, t) {
			return 0, TrapOverflow
		}
	case Div:
		if y == 0 {
			return 0, TrapDivZero
		}
		if y == -1 {
			if x == t.MinS() {
				return 0, TrapOverflow
			}
			r = -x
		} else {
			r = x / y
		}
	case Rem:
		if y == 0 {
			return 0, TrapDivZero
		}
		if y == -1 {
			r = 0
		} else {
			r = x % y
		}
	}
	return uint64(r), 0
}

// CastFits reports whether the value v of type from is representable in type to.
func CastFits(v uint64, from, to IntTy) bool {
	if from.Signed && int64(v) < 0 {
		return to.Signed && int64(v) >= to.MinS()
	}
	if to.Signed {
		return v <= uint64(to.MaxS())
	}
	return v <= to.MaxU()
}

func (m *vm) exec(b []Inst) bool {
	for _, in := range b {
		m.steps++
		switch s := in.(type) {
		case Mov:
			m.vars[s.Dst] = m.val(s.A)
		case Bin:
			r, t := EvalBin(s.Op, m.val(s.A), m.val(s.B), s.Ty)
			if t != 0 {
				return m.fail(t)
			}
			m.vars[s.Dst] = r
		case Neg:
			x := int64(m.val(s.A))
			if x == s.Ty.MinS() {
				return m.fail(TrapOverflow)
			}
			m.vars[s.Dst] = uint64(-x)
		case Not:
			m.vars[s.Dst] = m.val(s.A) ^ 1
		case Cast:
			v := m.val(s.A)
			if !CastFits(v, s.From, s.To) {
				return m.fail(TrapOverflow)
			}
			m.vars[s.Dst] = v
		case Clamp:
			v, lo, hi := m.val(s.A), m.val(s.Lo), m.val(s.Hi)
			var below, above bool
			if s.Ty.Signed {
				below, above = int64(v) < int64(lo), int64(v) > int64(hi)
			} else {
				below, above = v < lo, v > hi
			}
			switch {
			case below && s.TrapMode, above && s.TrapMode:
				return m.fail(TrapRequire)
			case below:
				v = lo
			case above:
				v = hi
			}
			m.vars[s.Dst] = v
		case Load:
			i := m.val(s.Idx)
			if i >= uint64(len(m.arrs[s.Arr])) {
				return m.fail(TrapIndex)
			}
			m.vars[s.Dst] = m.arrs[s.Arr][i]
		case Store:
			i := m.val(s.Idx)
			if i >= uint64(len(m.arrs[s.Arr])) {
				return m.fail(TrapIndex)
			}
			m.arrs[s.Arr][i] = m.val(s.A)
		case ArrInit:
			for i, v := range s.Vals {
				m.arrs[s.Arr][i] = m.val(v)
			}
		case TLoad:
			i := m.val(s.Idx)
			tab := m.p.Tables[s.Tab].Vals
			if i >= uint64(len(tab)) {
				return m.fail(TrapIndex)
			}
			m.vars[s.Dst] = tab[i]
		case BufLoad:
			i := m.val(s.Idx)
			if i >= m.val(s.Len) {
				return m.fail(TrapIndex)
			}
			m.vars[s.Dst] = uint64(m.bufs[s.Buf][i])
		case FBin:
			m.vars[s.Dst] = EvalFBin(s.Op, m.val(s.A), m.val(s.B), s.F32)
		case FUn:
			m.vars[s.Dst] = EvalFUn(s.Op, m.val(s.A), s.F32)
		case FCvt:
			switch s.Kind {
			case CvtIntToFloat:
				m.vars[s.Dst] = IntToFloat(m.val(s.A), s.Int, s.F32)
			case CvtFloatToInt:
				v, ok := FloatToInt(m.val(s.A), s.F32, s.Int)
				if !ok {
					return m.fail(TrapOverflow)
				}
				m.vars[s.Dst] = v
			default:
				m.vars[s.Dst] = FloatToFloat(m.val(s.A), s.F32)
			}
		case WriteFloat:
			m.out.WriteString(FormatFloat(ToFloat64(m.val(s.A), s.F32)))
		case ReadFloat:
			v, ok := ReadFloatFrom(m.in, s.F32)
			if !ok {
				return m.fail(TrapInput)
			}
			m.vars[s.Dst] = v
		case StrBuild:
			buf := m.bufs[s.Buf]
			pos := 0
			for _, q := range s.Parts {
				switch q.Kind {
				case 0:
					pos += copy(buf[pos:], q.S)
				case 1:
					pos += copy(buf[pos:], m.bufs[q.Buf][:m.val(q.Len)])
				case 3:
					pos += copy(buf[pos:], FormatFloat(ToFloat64(m.val(q.A), q.F32)))
				default:
					var txt string
					if q.Ty.Signed {
						txt = strconv.FormatInt(int64(m.val(q.A)), 10)
					} else {
						txt = strconv.FormatUint(m.val(q.A), 10)
					}
					pos += copy(buf[pos:], txt)
				}
			}
			m.vars[s.Len] = uint64(pos)
		case BufEq:
			la, lb := m.val(s.LenA), m.val(s.LenB)
			if la == lb && string(m.bufs[s.BufA][:la]) == string(m.bufs[s.BufB][:lb]) {
				m.vars[s.Dst] = 1
			} else {
				m.vars[s.Dst] = 0
			}
		case StrEq:
			n := m.val(s.Len)
			if n == uint64(len(s.S)) && string(m.bufs[s.Buf][:n]) == s.S {
				m.vars[s.Dst] = 1
			} else {
				m.vars[s.Dst] = 0
			}
		case ArrFill:
			v := m.val(s.Val)
			for i := range m.arrs[s.Arr] {
				m.arrs[s.Arr][i] = v
			}
		case ArrCopy:
			copy(m.arrs[s.Dst], m.arrs[s.Src])
		case ReadLine:
			n := 0
			buf := m.bufs[s.Buf]
			for {
				c, err := m.in.ReadByte()
				if err != nil || c == '\n' {
					break
				}
				if n < s.Cap {
					buf[n] = c
					n++
				}
			}
			m.vars[s.Len] = uint64(n)
		case ReadInt:
			v, ok := m.readInt(s.Ty)
			if !ok {
				return m.fail(TrapInput)
			}
			m.vars[s.Dst] = v
		case WriteStr:
			m.out.WriteString(s.S)
		case WriteBuf:
			m.out.Write(m.bufs[s.Buf][:m.val(s.Len)])
		case WriteDec:
			if s.Ty.Signed {
				m.out.WriteString(strconv.FormatInt(int64(m.val(s.A)), 10))
			} else {
				m.out.WriteString(strconv.FormatUint(m.val(s.A), 10))
			}
		case Alloc:
			m.vars[s.Dst] = m.tops[s.Arena]
			m.tops[s.Arena] += m.val(s.Size)
		case Mark:
			m.vars[s.Slot] = m.tops[s.Arena]
		case Reset:
			m.tops[s.Arena] = m.vars[s.Slot]
		case If:
			var stop bool
			if m.val(s.Cond) != 0 {
				stop = m.exec(s.Then)
			} else {
				stop = m.exec(s.Else)
			}
			if stop {
				return true
			}
		case While:
			count := 0
			for {
				if m.exec(s.Pre) {
					return true
				}
				if m.val(s.Cond) == 0 {
					break
				}
				if s.Bound >= 0 && count >= s.Bound {
					return m.fail(TrapBound)
				}
				if m.exec(s.Body) {
					return true
				}
				count++
			}
		case Ret:
			m.has = s.Has
			if s.Has {
				m.ret = m.val(s.A)
			}
			m.done = true
			return true
		case Trap:
			return m.fail(s.Code)
		}
	}
	return false
}

func (m *vm) readInt(t IntTy) (uint64, bool) { return ReadIntFrom(m.in, t) }

// ReadIntFrom parses a decimal integer token from in, exactly as the generated
// runtime does. ok is false for EOF, malformed input or out-of-range values.
func ReadIntFrom(in *bufio.Reader, t IntTy) (uint64, bool) {
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
		if !t.Signed {
			return 0, false
		}
		neg = true
		c, err = in.ReadByte()
		if err != nil {
			return 0, false
		}
	}
	if c < '0' || c > '9' {
		return 0, false
	}
	var mag uint64
	for {
		d := uint64(c - '0')
		if mag > (^uint64(0)-d)/10 {
			return 0, false
		}
		mag = mag*10 + d
		c, err = in.ReadByte()
		if err != nil {
			break
		}
		if c < '0' || c > '9' {
			in.UnreadByte()
			break
		}
	}
	if t.Signed {
		if neg {
			if mag > uint64(t.MaxS())+1 {
				return 0, false
			}
			return uint64(-int64(mag)), true
		}
		if mag > uint64(t.MaxS()) {
			return 0, false
		}
		return mag, true
	}
	if mag > t.MaxU() {
		return 0, false
	}
	return mag, true
}
