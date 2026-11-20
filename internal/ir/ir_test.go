package ir

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

var (
	u8  = IntTy{8, false}
	i8  = IntTy{8, true}
	u32 = IntTy{32, false}
	i32 = IntTy{32, true}
	u64 = IntTy{64, false}
	i64 = IntTy{64, true}
)

func s(v int64) uint64 { return uint64(v) }

func TestEvalBinTable(t *testing.T) {
	cases := []struct {
		name string
		op   Op
		a, b uint64
		ty   IntTy
		want uint64
		trap TrapCode
	}{
		{"u8 add ok", Add, 200, 55, u8, 255, 0},
		{"u8 add overflow", Add, 200, 56, u8, 0, TrapOverflow},
		{"u8 sub underflow", Sub, 3, 4, u8, 0, TrapOverflow},
		{"u8 mul overflow", Mul, 16, 16, u8, 0, TrapOverflow},
		{"i8 add overflow", Add, 127, 1, i8, 0, TrapOverflow},
		{"i8 add negative ok", Add, s(-128), 0, i8, s(-128), 0},
		{"i8 sub overflow", Sub, s(-128), 1, i8, 0, TrapOverflow},
		{"i8 mul ok", Mul, s(-8), 16, i8, s(-128), 0},
		{"i8 mul overflow", Mul, s(-8), 17, i8, 0, TrapOverflow},
		{"i8 div min/-1", Div, s(-128), s(-1), i8, 0, TrapOverflow},
		{"i8 rem min/-1", Rem, s(-128), s(-1), i8, 0, 0},
		{"u32 mul max*max overflows", Mul, math.MaxUint32, math.MaxUint32, u32, 0, TrapOverflow},
		{"u64 add overflow", Add, math.MaxUint64, 1, u64, 0, TrapOverflow},
		{"u64 add ok", Add, math.MaxUint64 - 1, 1, u64, math.MaxUint64, 0},
		{"u64 mul overflow", Mul, 1 << 32, 1 << 32, u64, 0, TrapOverflow},
		{"u64 mul ok", Mul, 1 << 31, 1 << 32, u64, 1 << 63, 0},
		{"i64 add overflow", Add, math.MaxInt64, 1, i64, 0, TrapOverflow},
		{"i64 sub overflow", Sub, s(math.MinInt64), 1, i64, 0, TrapOverflow},
		{"i64 mul overflow", Mul, s(math.MinInt64), s(-1), i64, 0, TrapOverflow},
		{"i64 mul ok neg", Mul, s(-3), 7, i64, s(-21), 0},
		{"i64 div min/-1", Div, s(math.MinInt64), s(-1), i64, 0, TrapOverflow},
		{"div by zero u32", Div, 5, 0, u32, 0, TrapDivZero},
		{"rem by zero i32", Rem, 5, 0, i32, 0, TrapDivZero},
		{"i32 div truncates toward zero", Div, s(-7), 2, i32, s(-3), 0},
		{"i32 rem sign follows dividend", Rem, s(-7), 2, i32, s(-1), 0},
		{"i32 rem negative divisor", Rem, 7, s(-2), i32, 1, 0},
		{"shl wraps u8", Shl, 0x81, 1, u8, 0x02, 0},
		{"shl wraps i8 to negative", Shl, 0x41, 1, i8, s(-126), 0},
		{"shl amount too big", Shl, 1, 8, u8, 0, TrapShift},
		{"shr unsigned logical", Shr, 0x80, 7, u8, 1, 0},
		{"shr signed arithmetic", Shr, s(-128), 7, i8, s(-1), 0},
		{"shr amount too big", Shr, 1, 32, u32, 0, TrapShift},
		{"and", And, 0xF0, 0x3C, u8, 0x30, 0},
		{"or", Or, 0xF0, 0x0F, u8, 0xFF, 0},
		{"xor", Xor, 0xFF, 0x0F, u8, 0xF0, 0},
		{"lt signed", Lt, s(-1), 1, i32, 1, 0},
		{"lt unsigned", Lt, math.MaxUint32, 1, u32, 0, 0},
		{"ge signed", Ge, s(-5), s(-5), i32, 1, 0},
		{"eq", Eq, 3, 3, u32, 1, 0},
		{"ne", Ne, 3, 3, u32, 0, 0},
		{"min signed", Min, s(-4), 3, i32, s(-4), 0},
		{"max unsigned", Max, s(-4) & 0xFFFFFFFF, 3, u32, s(-4) & 0xFFFFFFFF, 0},
	}
	for _, c := range cases {
		got, trap := EvalBin(c.op, c.a, c.b, c.ty)
		if trap != c.trap || (trap == 0 && got != c.want) {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", c.name, int64(got), trap, int64(c.want), c.trap)
		}
	}
}

func TestCastFits(t *testing.T) {
	cases := []struct {
		v        uint64
		from, to IntTy
		want     bool
	}{
		{255, u32, u8, true}, {256, u32, u8, false}, {s(-1), i32, u8, false}, {200, u8, i8, false}, {127, u8, i8, true},
		{s(-128), i32, i8, true}, {s(-129), i32, i8, false}, {math.MaxUint64, u64, i64, false},
		{math.MaxInt64, u64, i64, true}, {s(-1), i64, i32, true}, {s(math.MinInt64), i64, i32, false},
		{0, u32, u64, true}, {s(-5), i32, i64, true}, {s(-5), i64, u64, false},
	}
	for _, c := range cases {
		if got := CastFits(c.v, c.from, c.to); got != c.want {
			t.Errorf("CastFits(%d, %v->%v) = %v, want %v", int64(c.v), c.from, c.to, got, c.want)
		}
	}
}

func run(p *Program, in string) (string, Result) {
	var out bytes.Buffer
	r := Run(p, strings.NewReader(in), &out)
	return out.String(), r
}

func TestVMLoopAndBound(t *testing.T) {
	// v0 = 0; while v0 < 3 { write_dec v0; v0 += 1 }
	build := func(bound int) *Program {
		return &Program{NVars: 2, Body: []Inst{
			Mov{Dst: 0, A: K(0)},
			While{Pre: []Inst{Bin{Dst: 1, Op: Lt, A: V(0), B: K(3), Ty: u32}}, Cond: V(1), Bound: bound,
				Body: []Inst{WriteDec{A: V(0), Ty: u32}, Bin{Dst: 0, Op: Add, A: V(0), B: K(1), Ty: u32, Check: true}}},
			Ret{},
		}}
	}
	out, r := run(build(3), "")
	if out != "012" || r.Trap != TrapNone {
		t.Errorf("bound 3: out=%q trap=%v", out, r.Trap)
	}
	out, r = run(build(2), "")
	if out != "01" || r.Trap != TrapBound {
		t.Errorf("bound 2: out=%q trap=%v, want trap bound", out, r.Trap)
	}
	_, r = run(build(-1), "")
	if r.Trap != TrapNone {
		t.Errorf("unchecked bound trapped")
	}
}

func TestVMArraysAndTraps(t *testing.T) {
	p := &Program{NVars: 3, Arrs: []ArrInfo{{N: 3, Ty: u32}}, Body: []Inst{
		ArrInit{Arr: 0, Vals: []Operand{K(10), K(20), K(30)}},
		ReadInt{Dst: 0, Ty: u32},
		Load{Dst: 1, Arr: 0, Idx: V(0), Check: true},
		WriteDec{A: V(1), Ty: u32},
		Ret{A: V(1), Has: true},
	}}
	out, r := run(p, "2")
	if out != "30" || r.Trap != 0 || r.Ret != 30 {
		t.Errorf("load: out=%q r=%+v", out, r)
	}
	if _, r = run(p, "3"); r.Trap != TrapIndex {
		t.Errorf("want index trap, got %v", r.Trap)
	}
	if _, r = run(p, "x"); r.Trap != TrapInput {
		t.Errorf("want input trap, got %v", r.Trap)
	}
	if _, r = run(p, "-1"); r.Trap != TrapInput {
		t.Errorf("negative into unsigned should be an input error, got %v", r.Trap)
	}
}

func TestReadIntParsing(t *testing.T) {
	cases := []struct {
		in   string
		ty   IntTy
		want uint64
		ok   bool
	}{
		{"  42\n", u32, 42, true}, {"-42", i32, s(-42), true}, {"4294967295", u32, math.MaxUint32, true},
		{"4294967296", u32, 0, false}, {"-2147483648", i32, s(-2147483648), true}, {"-2147483649", i32, 0, false},
		{"18446744073709551615", u64, math.MaxUint64, true}, {"18446744073709551616", u64, 0, false},
		{"-9223372036854775808", i64, s(math.MinInt64), true}, {"abc", u32, 0, false}, {"", u32, 0, false},
		{"-", i32, 0, false}, {"12abc", u32, 12, true}, {"256", u8, 0, false}, {"255", u8, 255, true},
	}
	for _, c := range cases {
		p := &Program{NVars: 1, Body: []Inst{ReadInt{Dst: 0, Ty: c.ty}, Ret{A: V(0), Has: true}}}
		_, r := run(p, c.in)
		if (r.Trap == TrapNone) != c.ok || (c.ok && r.Ret != c.want) {
			t.Errorf("read %q as %v: ret=%d trap=%v, want ok=%v val=%d", c.in, c.ty, int64(r.Ret), r.Trap, c.ok, int64(c.want))
		}
	}
}

func TestOptimizeDCEAndCompact(t *testing.T) {
	p := &Program{NVars: 10, Arrs: []ArrInfo{{N: 2, Ty: u32}, {N: 2, Ty: u32}}, Body: []Inst{
		Mov{Dst: 5, A: K(1)},                                         // dead
		Bin{Dst: 6, Op: Add, A: V(5), B: K(2), Ty: u32},              // dead (feeds only dead)
		Bin{Dst: 7, Op: Div, A: K(1), B: V(8), Ty: u32, Check: true}, // checked: must stay
		ReadInt{Dst: 8, Ty: u32},                                     // effect: must stay
		Mov{Dst: 9, A: K(7)},                                         // used below
		ArrInit{Arr: 0, Vals: []Operand{K(1), K(2)}},                 // array never loaded: dead
		ArrInit{Arr: 1, Vals: []Operand{K(3), K(4)}},                 // loaded: stays
		Load{Dst: 3, Arr: 1, Idx: K(0)},                              // used
		WriteDec{A: V(3), Ty: u32},
		WriteDec{A: V(9), Ty: u32},
		Ret{},
	}}
	Optimize(p)
	text := Format(p)
	if strings.Contains(text, "add.u32") {
		t.Errorf("dead add not removed:\n%s", text)
	}
	if !strings.Contains(text, "div!.u32") || !strings.Contains(text, "read_int") {
		t.Errorf("effectful instructions removed:\n%s", text)
	}
	if len(p.Arrs) != 1 {
		t.Errorf("dead array not removed: %d arrays", len(p.Arrs))
	}
	if p.NVars > 5 {
		t.Errorf("variables not compacted: %d", p.NVars)
	}
	out, r := run(&Program{NVars: p.NVars, Arrs: p.Arrs, Body: []Inst{Mov{Dst: 0, A: K(0)}}}, "")
	_ = out
	_ = r
}

func TestClampAndRequire(t *testing.T) {
	mk := func(trap bool) *Program {
		return &Program{NVars: 2, Body: []Inst{
			ReadInt{Dst: 0, Ty: i32},
			Clamp{Dst: 1, A: V(0), Lo: K(s(-5)), Hi: K(5), Ty: i32, TrapMode: trap},
			WriteDec{A: V(1), Ty: i32}, Ret{},
		}}
	}
	for in, want := range map[string]string{"3": "3", "100": "5", "-100": "-5"} {
		if out, _ := run(mk(false), in); out != want {
			t.Errorf("clamp(%s) = %s, want %s", in, out, want)
		}
	}
	if _, r := run(mk(true), "100"); r.Trap != TrapRequire {
		t.Errorf("require should trap, got %v", r.Trap)
	}
	if out, r := run(mk(true), "4"); out != "4" || r.Trap != 0 {
		t.Errorf("require in range failed: %q %v", out, r.Trap)
	}
}

func TestArenaOps(t *testing.T) {
	p := &Program{NVars: 4, Arenas: []Arena{{"A", 100}}, Body: []Inst{
		Alloc{Dst: 0, Arena: 0, Size: K(10)},
		Mark{Slot: 1, Arena: 0},
		Alloc{Dst: 2, Arena: 0, Size: K(20)},
		Reset{Slot: 1, Arena: 0},
		Alloc{Dst: 3, Arena: 0, Size: K(5)},
		WriteDec{A: V(0), Ty: u32}, WriteStr{S: ","}, WriteDec{A: V(2), Ty: u32}, WriteStr{S: ","}, WriteDec{A: V(3), Ty: u32},
		Ret{},
	}}
	if out, _ := run(p, ""); out != "0,10,10" {
		t.Errorf("arena offsets = %q, want 0,10,10 (reset reuses space)", out)
	}
}

func TestFormatIsStable(t *testing.T) {
	p := &Program{NVars: 1, Body: []Inst{Mov{Dst: 0, A: K(5)}, If{Cond: V(0), Then: []Inst{WriteStr{S: "x"}}, Else: []Inst{Trap{Code: TrapIndex}}}, Ret{}}}
	a, b := Format(p), Format(p)
	if a != b || !strings.Contains(a, "trap index out of bounds") || !strings.Contains(a, "} else {") {
		t.Errorf("format output unexpected:\n%s", a)
	}
}

func TestTableBufAndStrEq(t *testing.T) {
	p := &Program{NVars: 5, Bufs: []int{8}, Tables: []Table{{Ty: u8, Vals: []uint64{7, 8, 9}}}, Body: []Inst{
		ReadLine{Buf: 0, Len: 0, Cap: 8},
		ReadInt{Dst: 1, Ty: u32},
		TLoad{Dst: 2, Tab: 0, Idx: V(1), Check: true},
		BufLoad{Dst: 3, Buf: 0, Idx: V(1), Len: V(0)},
		StrEq{Dst: 4, Buf: 0, Len: V(0), S: "hey"},
		WriteDec{A: V(2), Ty: u8}, WriteStr{S: ","}, WriteDec{A: V(3), Ty: u8}, WriteStr{S: ","}, WriteDec{A: V(4), Ty: u8},
		Ret{},
	}}
	if out, r := run(p, "hey\n2\n"); out != "9,121,1" || r.Trap != 0 {
		t.Errorf("hey/2: out=%q trap=%v", out, r.Trap)
	}
	if out, _ := run(p, "he\n1\n"); out != "8,101,0" {
		t.Errorf("he/1: out=%q (length mismatch must not equal)", out)
	}
	if _, r := run(p, "hey\n3\n"); r.Trap != TrapIndex {
		t.Errorf("table index 3 of 3 must trap, got %v", r.Trap)
	}
	if _, r := run(p, "ab\n2\n"); r.Trap != TrapIndex {
		t.Errorf("string index 2 of length 2 must trap, got %v", r.Trap)
	}
}

func TestOptimizePrunesUnusedTables(t *testing.T) {
	p := &Program{NVars: 2, Tables: []Table{{Ty: u8, Vals: []uint64{1}}, {Ty: u32, Vals: []uint64{5, 6}}}, Body: []Inst{
		TLoad{Dst: 0, Tab: 1, Idx: K(1)},
		WriteDec{A: V(0), Ty: u32},
		Ret{},
	}}
	Optimize(p)
	if len(p.Tables) != 1 || p.Tables[0].Ty != u32 || p.RodataBytes() != 8 {
		t.Fatalf("unused table not pruned: %+v", p.Tables)
	}
	if out, _ := run(p, ""); out != "6" {
		t.Errorf("remapped table index wrong: %q", out)
	}
}

func fbits(f float64) uint64   { return BitsOf64(f) }
func f32bits(f float32) uint64 { return BitsOf32(f) }

func TestFormatFloat(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	negZero := math.Copysign(0, -1)
	cases := []struct {
		v    float64
		want string
	}{
		{0, "0.000000"}, {negZero, "0.000000"}, {1.5, "1.500000"}, {-0.0000004, "0.000000"}, {-0.0000006, "-0.000001"},
		{0.0000015, "0.000002"}, {0.0000025, "0.000002"}, {0.0000005, "0.000000"}, // round-half-even on the scaled value
		{9e9, "9000000000.000000"}, {9.1e9, "big"}, {-9.1e9, "-big"}, {123456.7890125, "123456.789012"},
		{2.0 / 3.0, "0.666667"}, {0.1 + 0.2, "0.300000"}, {1e300, "big"}, {nan, "nan"}, {inf, "inf"}, {-inf, "-inf"},
	}
	for _, c := range cases {
		if got := FormatFloat(c.v); got != c.want {
			t.Errorf("FormatFloat(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestEvalFBin(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	f := func(op FOp, a, b float64) float64 { return F64Of(EvalFBin(op, fbits(a), fbits(b), false)) }
	x, y := 0.1, 0.2 // variables: Go folds constant expressions exactly, hardware does not
	if f(FAdd, x, y) != x+y || f(FAdd, x, y) == 0.3 || f(FDiv, 1, 0) != inf || f(FDiv, -1, 0) != -inf || !math.IsNaN(f(FDiv, 0, 0)) {
		t.Error("f64 arithmetic wrong")
	}
	cmp := func(op FOp, a, b float64) uint64 { return EvalFBin(op, fbits(a), fbits(b), false) }
	if cmp(FEq, nan, nan) != 0 || cmp(FNe, nan, nan) != 1 || cmp(FLt, nan, 1) != 0 || cmp(FGe, 1, nan) != 0 || cmp(FLe, 1, 1) != 1 {
		t.Error("NaN comparisons must be ordered (only != is true)")
	}
	if f(FMin, 3, nan) != 3 || !math.IsNaN(f(FMin, nan, 3)) || !math.IsNaN(f(FMax, nan, 3)) || f(FMax, 3, nan) != 3 {
		t.Error("min/max follow `if b < a { b } else { a }` (the first operand wins with NaN)")
	}
	a32, b32 := float32(0.1), float32(0.2)
	g := EvalFBin(FAdd, f32bits(a32), f32bits(b32), true)
	if F32Of(g) != a32+b32 || g>>32 != 0 {
		t.Errorf("f32 add must round to single precision and stay zero-extended: %#x", g)
	}
	m1, m2 := float32(16777216), float32(1.0000001)
	if F32Of(EvalFBin(FMul, f32bits(m1), f32bits(m2), true)) != m1*m2 {
		t.Error("f32 multiply")
	}
}

func TestEvalFUn(t *testing.T) {
	u := func(op FUOp, v float64) float64 { return F64Of(EvalFUn(op, fbits(v), false)) }
	if u(FSqrt, 2) != math.Sqrt2 || !math.IsNaN(u(FSqrt, -1)) || u(FFloor, -1.5) != -2 || u(FCeil, -1.5) != -1 || u(FTrunc, -1.5) != -1 || u(FAbs, -3) != 3 || u(FNeg, 2) != -2 {
		t.Error("unary float operations")
	}
	if EvalFUn(FIsNan, fbits(math.NaN()), false) != 1 || EvalFUn(FIsNan, fbits(1), false) != 0 {
		t.Error("is_nan")
	}
	if !math.Signbit(u(FCeil, -0.5)) {
		t.Error("ceil(-0.5) is -0")
	}
}

func TestFloatToInt(t *testing.T) {
	i32T, u32T, u64T := IntTy{32, true}, IntTy{32, false}, IntTy{64, false}
	cases := []struct {
		v    float64
		ty   IntTy
		want uint64
		ok   bool
	}{
		{2147483647.9, i32T, 2147483647, true}, {2147483648.0, i32T, 0, false},
		{-2147483648.9, i32T, uint64(0xFFFFFFFF80000000), true}, {-2147483649.0, i32T, 0, false},
		{-0.5, u32T, 0, true}, {-1.0, u32T, 0, false}, {4294967295.5, u32T, 4294967295, true}, {4294967296.0, u32T, 0, false},
		{1e19, u64T, 10000000000000000000, true}, {1.9e19, u64T, 0, false}, {math.NaN(), i32T, 0, false}, {math.Inf(1), u64T, 0, false},
	}
	for _, c := range cases {
		got, ok := FloatToInt(fbits(c.v), false, c.ty)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("FloatToInt(%v, %v) = (%#x, %v), want (%#x, %v)", c.v, c.ty, got, ok, c.want, c.ok)
		}
	}
}

func TestIntToFloatRounding(t *testing.T) {
	if got := F32Of(IntToFloat(16777217, IntTy{32, false}, true)); got != 16777216 {
		t.Errorf("16777217 as f32 = %v, want 16777216 (round to nearest even)", got)
	}
	if got := F64Of(IntToFloat(uint64(1)<<63+1, IntTy{64, false}, false)); got != 9223372036854775808.0 {
		t.Errorf("large u64 as f64 = %v", got)
	}
	if got := F64Of(IntToFloat(uint64(0xFFFFFFFFFFFFFFFF), IntTy{64, true}, false)); got != -1 {
		t.Errorf("-1i64 as f64 = %v", got)
	}
}

func TestReadFloatParsing(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"3.25", 3.25, true}, {"  -0.5\n", -0.5, true}, {"7", 7, true}, {"000.100", 0.1, true}, {"123456789012345", 123456789012345, true},
		{"1234567890123456", 0, false}, {"5.", 0, false}, {".5", 0, false}, {"abc", 0, false}, {"-", 0, false}, {"", 0, false},
		{"1.25.5", 1.25, true}, {"99999999.9999999", 99999999.9999999, true},
	}
	for _, c := range cases {
		p := &Program{NVars: 1, Body: []Inst{ReadFloat{Dst: 0}, Ret{A: V(0), Has: true}}}
		_, r := run(p, c.in)
		if (r.Trap == TrapNone) != c.ok || (c.ok && F64Of(r.Ret) != c.want) {
			t.Errorf("read %q: ret=%v trap=%v, want ok=%v value=%v", c.in, F64Of(r.Ret), r.Trap, c.ok, c.want)
		}
	}
}

func TestFloatInstructionsInVM(t *testing.T) {
	// v0 = 2.0 ; v1 = sqrt(v0) ; v2 = (i32)(v1 * 100.0)
	p := &Program{NVars: 4, Body: []Inst{
		Mov{Dst: 0, A: K(fbits(2))},
		FUn{Dst: 1, Op: FSqrt, A: V(0)},
		FBin{Dst: 2, Op: FMul, A: V(1), B: K(fbits(100))},
		FCvt{Dst: 3, A: V(2), Kind: CvtFloatToInt, Int: IntTy{32, true}},
		WriteDec{A: V(3), Ty: IntTy{32, true}}, WriteStr{S: " "}, WriteFloat{A: V(1)}, Ret{},
	}}
	if out, r := run(p, ""); out != "141 1.414214" || r.Trap != 0 {
		t.Errorf("out=%q trap=%v", out, r.Trap)
	}
	bad := &Program{NVars: 2, Body: []Inst{Mov{Dst: 0, A: K(fbits(3e9))}, FCvt{Dst: 1, A: V(0), Kind: CvtFloatToInt, Int: IntTy{32, true}}, Ret{}}}
	if _, r := run(bad, ""); r.Trap != TrapOverflow {
		t.Errorf("out-of-range float->int must trap, got %v", r.Trap)
	}
}
