package bench

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"apus/internal/driver"
	"apus/internal/ir"
	"apus/internal/peval"
)

const aosTmpl = `struct P { x: u32, y: u32 }
fn main() io {
    let n = io::read_u32();
    let mut ps: [P; %[1]d] = [P { x: 0, y: 0 }; %[1]d];
    let mut i: u32 = 0;
    while i < n bound(1000000) {
        let k = ((i * 7) %% %[1]d) as u64;
        ps[k].x = ps[k].x + 1;
        ps[k].y = ps[k].y + i;
        i += 1;
    }
    io::println(str(ps[0].x + ps[0].y));
}
`

const soaTmpl = `fn main() io {
    let n = io::read_u32();
    let mut xs: [u32; %[1]d] = [0; %[1]d];
    let mut ys: [u32; %[1]d] = [0; %[1]d];
    let mut i: u32 = 0;
    while i < n bound(1000000) {
        let k = ((i * 7) %% %[1]d) as u64;
        xs[k] = xs[k] + 1;
        ys[k] = ys[k] + i;
        i += 1;
    }
    io::println(str(xs[0] + ys[0]));
}
`

// TestDynCost reports the cost of dynamic-index writes into arrays of structs
// against the hand-written struct-of-arrays equivalent. Run with -v.
func TestDynCost(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	t.Logf("%6s | %-18s | %-18s | %s", "N", "AoS ir / vm", "SoA ir / vm", "slowdown")
	for _, n := range []int{4, 16, 64, 256} {
		var cells [2]string
		var vm [2]time.Duration
		for k, tmpl := range []string{aosTmpl, soaTmpl} {
			src := fmt.Sprintf(tmpl, n)
			out, derr := driver.Compile(src, peval.DefaultOptions())
			if derr != nil {
				cells[k] = "rejected " + derr.Code
				continue
			}
			lines := strings.Count(ir.Format(out.Res.Prog), "\n")
			best := time.Hour
			for r := 0; r < 3; r++ {
				var buf bytes.Buffer
				start := time.Now()
				ir.Run(out.Res.Prog, strings.NewReader("20000\n"), &buf)
				if d := time.Since(start); d < best {
					best = d
				}
			}
			vm[k] = best
			cells[k] = fmt.Sprintf("%5d / %-9s", lines, best.Round(time.Microsecond))
		}
		slow := "n/a"
		if vm[0] > 0 && vm[1] > 0 {
			slow = fmt.Sprintf("%.1fx", float64(vm[0])/float64(vm[1]))
		}
		t.Logf("%6d | %-18s | %-18s | %s", n, cells[0], cells[1], slow)
	}
}
