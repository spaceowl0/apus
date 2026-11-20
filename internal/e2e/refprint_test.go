package e2e

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"apus/internal/driver"
	"apus/internal/peval"
	"apus/internal/ref"
)

// TestRefPrint is a dev helper: it prints what the reference interpreter (which does no
// partial evaluation) produces, for writing independent goldens. Usage:
//
//	APUS_REF_FILE=prog.apus APUS_REF_STDIN="3 4;0 1" go test ./internal/e2e -run RefPrint -v
//
// Inputs are separated by ";" and words within an input by spaces (each becomes a line).
func TestRefPrint(t *testing.T) {
	f := os.Getenv("APUS_REF_FILE")
	if f == "" {
		t.Skip()
	}
	src, _ := os.ReadFile(f)
	out, derr := driver.Compile(string(src), peval.DefaultOptions())
	if derr != nil {
		t.Fatal(derr)
	}
	for _, in := range strings.Split(os.Getenv("APUS_REF_STDIN"), ";") {
		var b bytes.Buffer
		r := ref.RunLimited(out.Info, strings.NewReader(strings.ReplaceAll(in, " ", "\n")+"\n"), &b, 5_000_000)
		fmt.Printf("REF[%s] => %q trap=%v\n", in, b.String(), r.Trap)
	}
}
