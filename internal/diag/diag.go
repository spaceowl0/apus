// Package diag defines compiler diagnostics. Compiler phases report errors by
// panicking with *Error; Try converts that panic back into a returned error.
package diag

import (
	"fmt"
	"strings"
)

type Pos struct{ Line, Col int }

func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

type Error struct {
	Code string
	Msg  string
	Help string
	Pos  Pos
}

func (e *Error) Error() string {
	return fmt.Sprintf("error[%s]: %s (at %s)", e.Code, e.Msg, e.Pos)
}

func Errf(code string, pos Pos, format string, a ...any) *Error {
	return &Error{Code: code, Pos: pos, Msg: fmt.Sprintf(format, a...)}
}

func (e *Error) WithHelp(h string) *Error { e.Help = h; return e }

// Fail aborts the current compiler phase.
func Fail(e *Error) { panic(e) }

// Try runs f and returns any *Error raised via Fail.
func Try(f func()) (err *Error) {
	defer func() {
		if r := recover(); r != nil {
			if de, ok := r.(*Error); ok {
				err = de
				return
			}
			panic(r)
		}
	}()
	f()
	return nil
}

// Render formats e rustc-style with a source excerpt.
func Render(e *Error, file, src string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "error[%s]: %s\n  --> %s:%d:%d\n", e.Code, e.Msg, file, e.Pos.Line, e.Pos.Col)
	lines := strings.Split(src, "\n")
	if e.Pos.Line >= 1 && e.Pos.Line <= len(lines) {
		ln := lines[e.Pos.Line-1]
		fmt.Fprintf(&b, "   | %s\n", strings.ReplaceAll(ln, "\t", "    "))
		if e.Pos.Col >= 1 {
			fmt.Fprintf(&b, "   | %s^\n", strings.Repeat(" ", e.Pos.Col-1))
		}
	}
	if e.Help != "" {
		fmt.Fprintf(&b, "   = help: %s\n", e.Help)
	}
	return b.String()
}
