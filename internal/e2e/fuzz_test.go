package e2e

import (
	"bytes"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"

	"apus/internal/diag"
	"apus/internal/driver"
	"apus/internal/ir"
	"apus/internal/peval"
	"apus/internal/ref"
)

// gen builds random, well-typed Apus programs. Programs read integer inputs,
// compute with them through nested control flow, arrays and helper functions,
// and print results. Behaviour (stdout + trap) must be identical between the
// reference interpreter and the partially evaluated program.
type gen struct {
	r       *rand.Rand
	b       strings.Builder
	scopes  []map[string]variable
	nvar    int
	inputs  []string // types of read_* calls in order of appearance
	arrays  []string
	loopVar int
	inLoop  int
	fvars   []string
}

type variable struct {
	name string
	ty   string
	mut  bool
}

func newGen(seed int64) *gen {
	return &gen{r: rand.New(rand.NewSource(seed))}
}

func (g *gen) w(f string, a ...any)  { fmt.Fprintf(&g.b, f, a...) }
func (g *gen) push()                 { g.scopes = append(g.scopes, map[string]variable{}) }
func (g *gen) pop()                  { g.scopes = g.scopes[:len(g.scopes)-1] }
func (g *gen) chance(p float64) bool { return g.r.Float64() < p }

func (g *gen) vars(ty string, mutOnly bool) []variable {
	var out []variable
	for _, s := range g.scopes {
		for _, v := range s {
			if v.ty == ty && (!mutOnly || v.mut) {
				out = append(out, v)
			}
		}
	}
	// deterministic order
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].name < out[j-1].name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (g *gen) declare(ty string, mut bool) variable {
	g.nvar++
	v := variable{fmt.Sprintf("v%d", g.nvar), ty, mut}
	g.scopes[len(g.scopes)-1][v.name] = v
	return v
}

func (g *gen) lit(ty string) string {
	vals := []int{0, 1, 2, 3, 5, 7, 10, 100, -1, -3, -7, 255, 1000, 65536}
	n := vals[g.r.Intn(len(vals))]
	if ty == "u32" && n < 0 {
		n = -n
	}
	return strconv.Itoa(n)
}

func (g *gen) intType() string {
	if g.chance(0.7) {
		return "i32"
	}
	return "u32"
}

func (g *gen) expr(ty string, depth int) string {
	if ty == "bool" {
		return g.boolExpr(depth)
	}
	if depth <= 0 || g.chance(0.2) {
		vs := g.vars(ty, false)
		if len(vs) > 0 && g.chance(0.8) {
			return vs[g.r.Intn(len(vs))].name
		}
		return g.lit(ty)
	}
	switch g.r.Intn(12) {
	case 0, 1, 2, 3:
		ops := []string{"+", "-", "*", "&", "|", "^"}
		return fmt.Sprintf("(%s %s %s)", g.expr(ty, depth-1), ops[g.r.Intn(len(ops))], g.expr(ty, depth-1))
	case 4:
		op := "/"
		if g.chance(0.5) {
			op = "%"
		}
		// bias the divisor away from zero
		d := g.expr(ty, depth-1)
		if g.chance(0.7) {
			d = fmt.Sprintf("max(%s, 1)", d)
		}
		return fmt.Sprintf("(%s %s %s)", g.expr(ty, depth-1), op, d)
	case 5:
		op := "<<"
		if g.chance(0.5) {
			op = ">>"
		}
		return fmt.Sprintf("(%s %s %d)", g.expr(ty, depth-1), op, g.r.Intn(8))
	case 6:
		if ty == "i32" {
			if g.chance(0.5) {
				return fmt.Sprintf("abs(%s)", g.expr(ty, depth-1))
			}
			return fmt.Sprintf("(-%s)", g.expr(ty, depth-1))
		}
		return g.expr(ty, depth-1)
	case 7:
		lo, hi := g.r.Intn(5), 5+g.r.Intn(200)
		return fmt.Sprintf("clamp(%s, %d, %d)", g.expr(ty, depth-1), lo, hi)
	case 8:
		f := "min"
		if g.chance(0.5) {
			f = "max"
		}
		return fmt.Sprintf("%s(%s, %s)", f, g.expr(ty, depth-1), g.expr(ty, depth-1))
	case 9:
		other := "u32"
		if ty == "u32" {
			other = "i32"
		}
		return fmt.Sprintf("(clamp(%s, 0, 1000) as %s)", g.expr(other, depth-1), ty)
	case 10:
		if ty == "i32" && len(g.arrays) > 0 {
			a := g.arrays[g.r.Intn(len(g.arrays))]
			return fmt.Sprintf("%s[%s]", a, g.index(depth-1))
		}
		return g.expr(ty, depth-1)
	default:
		if ty == "i32" {
			return fmt.Sprintf("h%d(%s, %s)", g.r.Intn(4), g.expr("i32", depth-1), g.expr("i32", depth-1))
		}
		return g.expr(ty, depth-1)
	}
}

// fexpr generates an f64 expression over float variables, int variables and literals.
func (g *gen) fexpr(depth int) string {
	if depth <= 0 || g.chance(0.25) {
		if len(g.fvars) > 0 && g.chance(0.7) {
			return g.fvars[g.r.Intn(len(g.fvars))]
		}
		if iv := g.vars("i32", false); len(iv) > 0 && g.chance(0.3) {
			return fmt.Sprintf("(%s as f64)", iv[g.r.Intn(len(iv))].name)
		}
		lits := []string{"0.5", "1.0", "2.0", "0.1", "3.14159", "100.0", "0.0", "7.25", "1e3", "0.001"}
		return lits[g.r.Intn(len(lits))]
	}
	switch g.r.Intn(9) {
	case 0, 1, 2, 3:
		ops := []string{"+", "-", "*", "/"}
		return fmt.Sprintf("(%s %s %s)", g.fexpr(depth-1), ops[g.r.Intn(len(ops))], g.fexpr(depth-1))
	case 4:
		return fmt.Sprintf("(-%s)", g.fexpr(depth-1))
	case 5:
		fn := []string{"sqrt", "abs", "floor", "ceil", "trunc"}[g.r.Intn(5)]
		return fmt.Sprintf("%s(%s)", fn, g.fexpr(depth-1))
	case 6:
		fn := []string{"min", "max"}[g.r.Intn(2)]
		return fmt.Sprintf("%s(%s, %s)", fn, g.fexpr(depth-1), g.fexpr(depth-1))
	case 7:
		return fmt.Sprintf("((%s as f32) as f64)", g.fexpr(depth-1))
	}
	return fmt.Sprintf("((min(max(%s, -1000.0), 1000.0) as i32) as f64)", g.fexpr(depth-1))
}

func (g *gen) index(depth int) string {
	if g.chance(0.4) {
		return strconv.Itoa(g.r.Intn(4))
	}
	return fmt.Sprintf("(clamp(%s, 0, 3) as u64)", g.expr("i32", depth))
}

// index3 is an index into the 3x3 array; occasionally 3, which must trap identically.
func (g *gen) index3(depth int) string {
	if g.chance(0.4) {
		return strconv.Itoa(g.r.Intn(3))
	}
	hi := 2
	if g.chance(0.08) {
		hi = 3
	}
	return fmt.Sprintf("(clamp(%s, 0, %d) as u64)", g.expr("i32", depth), hi)
}

func (g *gen) boolExpr(depth int) string {
	if depth <= 0 || g.chance(0.3) {
		vs := g.vars("bool", false)
		if len(vs) > 0 && g.chance(0.5) {
			return vs[g.r.Intn(len(vs))].name
		}
		ty := g.intType()
		ops := []string{"<", "<=", ">", ">=", "==", "!="}
		return fmt.Sprintf("(%s %s %s)", g.expr(ty, depth-1), ops[g.r.Intn(len(ops))], g.expr(ty, depth-1))
	}
	switch g.r.Intn(4) {
	case 0:
		return fmt.Sprintf("(%s && %s)", g.boolExpr(depth-1), g.boolExpr(depth-1))
	case 1:
		return fmt.Sprintf("(%s || %s)", g.boolExpr(depth-1), g.boolExpr(depth-1))
	case 2:
		return fmt.Sprintf("(!%s)", g.boolExpr(depth-1))
	}
	ty := g.intType()
	ops := []string{"<", "<=", ">", ">=", "==", "!="}
	return fmt.Sprintf("(%s %s %s)", g.expr(ty, depth-1), ops[g.r.Intn(len(ops))], g.expr(ty, depth-1))
}

func (g *gen) input(ty string) string {
	g.inputs = append(g.inputs, ty)
	raw := fmt.Sprintf("io::read_%s()", ty)
	if g.chance(0.75) {
		if ty == "u32" {
			return fmt.Sprintf("clamp(%s, 0, %d)", raw, []int{3, 10, 100, 5000}[g.r.Intn(4)])
		}
		return fmt.Sprintf("clamp(%s, -%d, %d)", raw, []int{3, 10, 100}[g.r.Intn(3)], []int{3, 10, 100}[g.r.Intn(3)])
	}
	return raw
}

func (g *gen) stmts(n, depth int, ind string) {
	g.push()
	for i := 0; i < n; i++ {
		g.stmt(depth, ind)
	}
	g.pop()
}

func (g *gen) stmt(depth int, ind string) {
	k := g.r.Intn(19)
	if depth <= 0 && k >= 4 {
		k = g.r.Intn(4)
	}
	switch k {
	case 0, 1:
		ty := []string{"i32", "i32", "u32", "bool"}[g.r.Intn(4)]
		mut := g.chance(0.7)
		e := g.expr(ty, 3)
		v := g.declare(ty, mut)
		m := ""
		if mut {
			m = "mut "
		}
		g.w("%slet %s%s: %s = %s;\n", ind, m, v.name, ty, e)
	case 2, 3:
		ty := []string{"i32", "i32", "u32", "bool"}[g.r.Intn(4)]
		vs := g.vars(ty, true)
		if len(vs) == 0 {
			g.stmt(depth, ind)
			return
		}
		g.w("%s%s = %s;\n", ind, vs[g.r.Intn(len(vs))].name, g.expr(ty, 3))
	case 4, 5:
		g.w("%sif %s {\n", ind, g.boolExpr(2))
		g.stmts(1+g.r.Intn(3), depth-1, ind+"    ")
		if g.chance(0.6) {
			g.w("%s} else {\n", ind)
			g.stmts(1+g.r.Intn(3), depth-1, ind+"    ")
		}
		g.w("%s}\n", ind)
	case 6:
		g.loopVar++
		iv := fmt.Sprintf("i%d", g.loopVar)
		g.w("%sfor %s in 0..%d {\n", ind, iv, 1+g.r.Intn(5))
		g.push()
		g.inLoop++
		g.scopes[len(g.scopes)-1][iv] = variable{iv, "i32", false}
		g.stmts(1+g.r.Intn(3), depth-1, ind+"    ")
		g.inLoop--
		g.pop()
		g.w("%s}\n", ind)
	case 7:
		g.loopVar++
		iv := fmt.Sprintf("i%d", g.loopVar)
		g.w("%sfor %s in 0..clamp((%s) as u32, 0, 6) {\n", ind, iv, g.expr("u32", 2))
		g.push()
		g.inLoop++
		g.scopes[len(g.scopes)-1][iv] = variable{iv, "u32", false}
		g.stmts(1+g.r.Intn(3), depth-1, ind+"    ")
		g.inLoop--
		g.pop()
		g.w("%s}\n", ind)
	case 8:
		g.loopVar++
		c := fmt.Sprintf("w%d", g.loopVar)
		g.w("%slet mut %s: u32 = 0;\n", ind, c)
		g.push()
		g.scopes[len(g.scopes)-1][c] = variable{c, "u32", false}
		lim := g.expr("u32", 2)
		g.w("%swhile %s < clamp((%s) as u32, 0, 6) bound(7) {\n", ind, c, lim)
		g.w("%s    %s += 1;\n", ind, c)
		g.inLoop++
		g.stmts(1+g.r.Intn(2), depth-1, ind+"    ")
		g.inLoop--
		g.w("%s}\n", ind)
		g.pop()
	case 10, 11:
		if g.inLoop > 0 {
			kw := "break"
			if g.chance(0.5) {
				kw = "continue"
			}
			g.w("%sif %s { %s; }\n", ind, g.boolExpr(2), kw)
		} else {
			g.stmt(depth, ind)
		}
	case 12:
		g.w("%smatch clamp(%s, 0, 6) {\n", ind, g.expr("i32", 2))
		g.w("%s    0 => {\n", ind)
		g.stmts(1+g.r.Intn(2), depth-1, ind+"        ")
		g.w("%s    }\n", ind)
		g.w("%s    1 | 2 => {\n", ind)
		g.stmts(1+g.r.Intn(2), depth-1, ind+"        ")
		g.w("%s    }\n", ind)
		g.w("%s    3..=4 => {\n", ind)
		g.stmts(1, depth-1, ind+"        ")
		g.w("%s    }\n", ind)
		g.w("%s    _ => {\n", ind)
		g.stmts(1+g.r.Intn(2), depth-1, ind+"        ")
		g.w("%s    }\n", ind)
		g.w("%s}\n", ind)
	case 13:
		scr, e1, e2, e3, e4 := g.expr("i32", 2), g.expr("i32", 2), g.expr("i32", 2), g.expr("i32", 2), g.expr("i32", 2)
		v := g.declare("i32", true)
		g.w("%slet mut %s: i32 = match clamp(%s, -3, 8) {\n%s    -3..=-1 => %s,\n%s    0 => %s,\n%s    1 | 2 | 3 => %s,\n%s    _ => %s,\n%s};\n",
			ind, v.name, scr, ind, e1, ind, e2, ind, e3, ind, e4, ind)
	case 14:
		g.w("%sps[%s].%s = %s;\n", ind, g.index(2), []string{"x", "y"}[g.r.Intn(2)], g.expr("i32", 3))
	case 15:
		g.w("%sps[%s] = Q { x: %s, y: %s };\n", ind, g.index(2), g.expr("i32", 2), g.expr("i32", 2))
	case 16:
		g.w("%sgr[%s][%s] = %s;\n", ind, g.index3(2), g.index3(2), g.expr("i32", 3))
	case 17:
		g.w("%sms = \"k\" ++ str(%s);\n", ind, g.expr("i32", 2))
	case 18:
		switch g.r.Intn(9) {
		case 7:
			g.w("%s{ let mut tt: i32 = 0; for q in ps { tt = max(min(tt + q.x, 1000000), -1000000) - min(max(q.y, -5), 5); } io::println(\"fe=\" ++ str(tt)); }\n", ind)
		case 8:
			g.w("%s{ let mut tg: i32 = 0; for row in gr { for c in row { tg = max(min(tg + c, 1000000), -1000000); } } io::println(\"fg=\" ++ str(tg)); }\n", ind)
		case 6:
			g.w("%sio::println(\"b=\" ++ str(%s) ++ str(%s));\n", ind, g.boolExpr(2), g.boolExpr(2))
		case 4:
			g.w("%s{ let h = hs(%s); io::println(h ++ \"!\"); }\n", ind, g.expr("i32", 2))
		case 5:
			g.w("%sms = hs(%s);\n", ind, g.expr("i32", 2))
		case 3:
			g.w("%s{ let cp = ps; ps[%s].x = %s; io::println(str(cp[%s].x) ++ \":\" ++ str(ps[%s].x)); if %s { ps = cp; } }\n", ind, g.index(2), g.expr("i32", 2), g.index(2), g.index(2), g.boolExpr(2))
		case 0:
			g.w("%s{ let t = ps[%s]; io::println(\"t=\" ++ str(t.x) ++ \",\" ++ str(t.y)); }\n", ind, g.index(2))
		case 1:
			g.w("%s{ let c = hq(%s, %s); io::println(\"c=\" ++ str(c.x) ++ \",\" ++ str(c.y)); }\n", ind, g.expr("i32", 2), g.expr("i32", 2))
		default:
			g.w("%s{ let snap = ms; ms = \"k\" ++ str(%s); io::println(snap ++ \"/\" ++ ms ++ str(gr[%s][%s])); }\n", ind, g.expr("i32", 2), g.index3(2), g.index3(2))
		}
	default:
		if len(g.arrays) > 0 {
			a := g.arrays[g.r.Intn(len(g.arrays))]
			g.w("%s%s[%s] = %s;\n", ind, a, g.index(2), g.expr("i32", 3))
		} else {
			g.w("%sio::println(\"x=\" ++ str(%s));\n", ind, g.expr("i32", 3))
		}
	}
}

const helpers = `struct Q { x: i32, y: i32 }

fn hs(a: i32) -> Str<8> {
    if a > 3 { return "big"; }
    let mut i: i32 = 0;
    while i < 3 bound(3) {
        if i == a { return "i" ++ str(i as u8); }
        i += 1;
    }
    return "none";
}

fn hq(a: i32, b: i32) -> Q {
    if a > b { return Q { x: a, y: b }; }
    let mut i: u32 = 0;
    while i < 3 bound(3) {
        if (i as i32) == clamp(b, 0, 5) { return Q { x: 1, y: (i as i32) }; }
        i += 1;
    }
    return Q { x: b, y: a };
}

fn h0(a: i32, b: i32) -> i32 {
    if a > b { return a - b; }
    if a < 0 { return 0; }
    return a + b;
}

pure fn h1(a: i32, b: i32) -> i32 {
    let mut r = a;
    for i in 0..3 {
        if r > b { r = r - 1; } else { r = r + 2; }
    }
    return r;
}

fn h3(a: i32, b: i32) -> i32 {
    for i in 0..4 {
        if a + i > b { return i; }
    }
    let mut j: u32 = 0;
    while j < 5 bound(5) {
        if (j as i32) * 3 > b { return -1 - (j as i32); }
        j += 1;
    }
    for k in 0..clamp(a, 0, 4) {
        if k * 2 > b { return k + 10; }
    }
    return 99;
}

fn h2(a: i32, b: i32) -> i32 {
    let mut x = clamp(a, 0, 20);
    let mut s = 0;
    while x > 0 bound(20) {
        s += x;
        x -= clamp(b, 1, 5);
    }
    return s;
}

`

func (g *gen) program() string {
	g.b.WriteString(helpers)
	g.w("fn main() io {\n")
	g.push()
	n := 2 + g.r.Intn(3)
	for i := 0; i < n; i++ {
		ty := g.intType()
		v := g.declare(ty, g.chance(0.8))
		m := ""
		if v.mut {
			m = "mut "
		}
		g.w("    let %s%s: %s = %s;\n", m, v.name, ty, g.input(ty))
	}
	nstr := g.r.Intn(3)
	var strs []string
	for i := 0; i < nstr; i++ {
		s := fmt.Sprintf("s%d", i)
		g.w("    let %s = io::read_line(6);\n", s)
		g.inputs = append(g.inputs, "line")
		strs = append(strs, s)
		iv := g.vars("i32", false)
		r := fmt.Sprintf("r%d", i)
		if len(iv) > 0 {
			g.w("    let %s = \"k\" ++ %s ++ str(%s);\n", r, s, iv[g.r.Intn(len(iv))].name)
		} else {
			g.w("    let %s = \"k\" ++ %s ++ str(len(%s));\n", r, s, s)
		}
		g.w("    io::println(%s);\n    io::println(str(len(%s)));\n", r, r)
		g.w("    if %s == \"ka1\" { io::println(\"hit\"); }\n", r)
		c := g.declare("u32", true)
		g.w("    let mut %s: u32 = 0;\n    if len(%s) > 0 { %s = %s[0] as u32; }\n", c.name, s, c.name, s)
		g.w("    if len(%s) > 1 { io::println(str(%s[len(%s) - 1] as u32)); }\n", r, r, r)
	}
	nf := g.r.Intn(3)
	for i := 0; i < nf; i++ {
		name := fmt.Sprintf("f%d", i)
		g.w("    let %s: f64 = io::read_f64();\n", name)
		g.inputs = append(g.inputs, "f64")
		g.fvars = append(g.fvars, name)
	}
	if len(g.fvars) > 0 {
		g.w("    io::println(str(%s));\n", g.fexpr(3))
		g.w("    let mut acc: f64 = 0.0;\n    for k in 0..4 { if (k as f64) < %s { acc += %s * (k as f64); } else { acc -= 0.5; } }\n    io::println(str(acc));\n", g.fexpr(2), g.fexpr(2))
		g.w("    let q: f32 = (%s) as f32;\n    io::println(str(q * q + 1.0) ++ \" \" ++ str(q as f64));\n", g.fexpr(2))
		g.w("    if %s <= %s { io::println(\"le\"); } else { io::println(\"gt\"); }\n", g.fexpr(2), g.fexpr(2))
		g.w("    io::println(str(%s as i32));\n", g.fexpr(2))
	}
	if len(strs) == 2 {
		g.w("    if %s == %s { io::println(\"eq\"); } else { io::println(\"ne\"); }\n", strs[0], strs[1])
	}
	for i := 0; i < g.r.Intn(3); i++ {
		name := fmt.Sprintf("arr%d", i)
		g.w("    let mut %s = [%s, %s, %s, %s];\n", name, g.lit("i32"), g.lit("i32"), g.expr("i32", 1), g.lit("i32"))
		g.arrays = append(g.arrays, name)
	}
	g.w("    let mut ps: [Q; 4] = [Q { x: 1, y: 2 }; 4];\n")
	g.w("    let mut gr: [[i32; 3]; 3] = [[0; 3]; 3];\n")
	g.w("    let mut ms: Str<12> = \"init\";\n")
	g.stmts(3+g.r.Intn(5), 3, "    ")
	g.w("    io::println(ms);\n")
	for k := 0; k < 4; k++ {
		g.w("    io::println(str(ps[%d].x) ++ \":\" ++ str(ps[%d].y));\n", k, k)
	}
	g.w("    io::println(str(gr[0][0]) ++ str(gr[0][1]) ++ str(gr[0][2]) ++ str(gr[1][0]) ++ str(gr[1][1]) ++ str(gr[1][2]) ++ str(gr[2][0]) ++ str(gr[2][1]) ++ str(gr[2][2]));\n")
	// print every top-level variable
	for _, v := range g.vars("i32", false) {
		g.w("    io::println(\"%s=\" ++ str(%s));\n", v.name, v.name)
	}
	for _, v := range g.vars("u32", false) {
		g.w("    io::println(\"%s=\" ++ str(%s));\n", v.name, v.name)
	}
	for _, a := range g.arrays {
		g.w("    io::println(str(%s[0]) ++ \",\" ++ str(%s[1]) ++ \",\" ++ str(%s[2]) ++ \",\" ++ str(%s[3]));\n", a, a, a, a)
	}
	g.pop()
	g.w("}\n")
	return g.b.String()
}

func randomInput(r *rand.Rand, tys []string) string {
	var sb strings.Builder
	for _, ty := range tys {
		if ty == "f64" {
			toks := []string{"0", "1", "-1", "2.5", "-0.25", "3.14159", "100", "0.001", "12345.678", "-7.75", "9999999.5", "0.0000005", "1000000000.25", "abc"}
			if r.Intn(20) != 0 {
				toks = toks[:len(toks)-1]
			}
			sb.WriteString(toks[r.Intn(len(toks))] + "\n")
			continue
		}
		if ty == "line" {
			alphabet := "a1xyz"
			n := r.Intn(9)
			for k := 0; k < n; k++ {
				sb.WriteByte(alphabet[r.Intn(len(alphabet))])
			}
			sb.WriteByte('\n')
			continue
		}
		special := []string{"0", "1", "2", "7", "100", "-1", "-5", "2147483647", "-2147483648", "4294967295", "99999999999"}
		var tok string
		if r.Intn(3) == 0 {
			tok = special[r.Intn(len(special))]
		} else if ty == "u32" {
			tok = strconv.Itoa(r.Intn(300))
		} else {
			tok = strconv.Itoa(r.Intn(300) - 150)
		}
		sb.WriteString(tok + "\n")
	}
	return sb.String()
}

func TestDifferentialFuzz(t *testing.T) {
	n := 300
	if s := os.Getenv("APUS_FUZZ_N"); s != "" {
		n, _ = strconv.Atoi(s)
	}
	nativeMax := 60
	if s := os.Getenv("APUS_FUZZ_NATIVE"); s != "" {
		nativeMax, _ = strconv.Atoi(s)
	}
	allowed := map[string]bool{"E0301": true, "E0303": true, "E0304": true, "E0308": true, "E0201": true, "E0502": true}
	compiled, skipped, traps, native := 0, 0, 0, 0
	reasons := map[string]int{}
	for seed := int64(1); seed <= int64(n); seed++ {
		g := newGen(seed)
		src := g.program()
		var out *driver.Output
		var derr *diag.Error
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("seed %d: compiler panicked: %v\n--- source ---\n%s", seed, r, src)
				}
			}()
			out, derr = driver.Compile(src, peval.DefaultOptions())
		}()
		if derr != nil {
			if !allowed[derr.Code] {
				t.Fatalf("seed %d: unexpected compile error %v\nline: %s", seed, derr, strings.Split(src, "\n")[derr.Pos.Line-1])
			}
			skipped++
			reasons[derr.Code]++
			if derr.Code == "E0502" {
				reasons["E0502: "+derr.Msg]++
			}
			continue
		}
		compiled++
		r := rand.New(rand.NewSource(seed * 7919))
		var exe string
		if clangPath != "" && native < nativeMax {
			exe = buildNative(t, out.Res.Prog, fmt.Sprintf("fuzz%d", seed))
			native++
		}
		for k := 0; k < 4; k++ {
			stdin := randomInput(r, g.inputs)
			var vmOut, refOut bytes.Buffer
			vr := ir.Run(out.Res.Prog, strings.NewReader(stdin), &vmOut)
			rr := ref.RunLimited(out.Info, strings.NewReader(stdin), &refOut, 5_000_000)
			if rr.Exceeded {
				continue
			}
			if vr.Trap != ir.TrapNone {
				traps++
			}
			if vmOut.String() != refOut.String() || vr.Trap != rr.Trap {
				t.Fatalf("seed %d: partial evaluation changed behaviour\nstdin=%q\nvm : %q trap=%v\nref: %q trap=%v\n--- source ---\n%s\n--- ir ---\n%s",
					seed, stdin, vmOut.String(), vr.Trap, refOut.String(), rr.Trap, src, ir.Format(out.Res.Prog))
			}
			if rr.Trap == ir.TrapNone && rr.Ret != nil && int(new(big.Int).And(rr.Ret, big.NewInt(255)).Int64()) != vr.ExitCode(out.Res.Prog) {
				t.Fatalf("seed %d: exit code mismatch", seed)
			}
			if exe != "" {
				nat := runNative(exe, stdin)
				if nat.stdout != vmOut.String() || nat.trap != vr.Trap {
					t.Fatalf("seed %d: native differs from vm\nstdin=%q\nnative: %q trap=%v\nvm: %q trap=%v\n--- source ---\n%s\n--- ir ---\n%s",
						seed, stdin, nat.stdout, nat.trap, vmOut.String(), vr.Trap, src, ir.Format(out.Res.Prog))
				}
			}
		}
	}
	t.Logf("fuzz: %d programs compiled, %d rejected at compile time, %d trapping runs, %d built natively; rejections: %v", compiled, skipped, traps, native, reasons)
	if compiled < n/3 {
		t.Errorf("too few programs compiled (%d of %d): generator is producing mostly-invalid programs", compiled, n)
	}
}
