package syntax

import "testing"

func TestFloatLexing(t *testing.T) {
	for src, want := range map[string]string{"1.5": "1.5", "0.25": "0.25", "1_000.5": "1000.5", "2.5e3": "2.5e3", "1e5": "1e5", "3.0E-2": "3.0E-2"} {
		toks := Lex(src)
		if toks[0].Kind != Float_ || toks[0].Text != want {
			t.Errorf("%q -> kind %v text %q, want float %q", src, toks[0].Kind, toks[0].Text, want)
		}
	}
	for _, src := range []string{"0..10", "5", "0xFE", "1_000"} {
		if Lex(src)[0].Kind == Float_ {
			t.Errorf("%q must not lex as a float", src)
		}
	}
	toks := Lex("0..3")
	if toks[0].Kind != Int_ || toks[1].Text != ".." || toks[2].Kind != Int_ {
		t.Errorf("range lexing broken: %+v", toks)
	}
}
