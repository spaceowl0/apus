package syntax

import (
	"math/big"
	"strings"

	"apus/internal/diag"
)

type TokKind int

const (
	EOF TokKind = iota
	Ident_
	Int_
	Float_
	Str_
	Op
)

type Token struct {
	Kind TokKind
	Text string
	Val  *big.Int
	Pos  diag.Pos
}

var ops3 = []string{"..="}
var ops2 = []string{"=>", "++", "..", "->", "::", "==", "!=", "<=", ">=", "&&", "||", "<<", ">>", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^="}

const ops1 = "+-*/%<>=!&|^(){}[];:,.#"

func Lex(src string) []Token {
	var toks []Token
	line, col := 1, 1
	i := 0
	adv := func(n int) {
		for k := 0; k < n; k++ {
			if src[i] == '\n' {
				line++
				col = 1
			} else {
				col++
			}
			i++
		}
	}
	for i < len(src) {
		c := src[i]
		pos := diag.Pos{Line: line, Col: col}
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			adv(1)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				adv(1)
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			adv(2)
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				adv(1)
			}
			if i+1 >= len(src) {
				diag.Fail(diag.Errf("E0001", pos, "unterminated block comment"))
			}
			adv(2)
		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && (isAlnum(src[j])) {
				j++
			}
			// a decimal point followed by a digit (not `..`) continues the literal as a float
			isHex := j-i > 1 && (src[i+1] == 'x' || src[i+1] == 'X')
			if !isHex && j < len(src)-1 && src[j] == '.' && src[j+1] >= '0' && src[j+1] <= '9' {
				j++
				for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '_') {
					j++
				}
			}
			if !isHex && j < len(src) && (src[j] == 'e' || src[j] == 'E') {
				k := j + 1
				if k < len(src) && (src[k] == '+' || src[k] == '-') {
					k++
				}
				if k < len(src) && src[k] >= '0' && src[k] <= '9' {
					for k < len(src) && src[k] >= '0' && src[k] <= '9' {
						k++
					}
					j = k
				}
			}
			if txt := src[i:j]; !isHex && strings.ContainsAny(txt, ".eE") {
				toks = append(toks, Token{Kind: Float_, Text: strings.ReplaceAll(txt, "_", ""), Pos: pos})
				adv(j - i)
				continue
			}
			txt := src[i:j]
			v, ok := new(big.Int).SetString(strings.ReplaceAll(txt, "_", ""), 0)
			if !ok {
				diag.Fail(diag.Errf("E0001", pos, "invalid integer literal %q", txt))
			}
			toks = append(toks, Token{Kind: Int_, Text: txt, Val: v, Pos: pos})
			adv(j - i)
		case isAlpha(c):
			j := i
			for j < len(src) && isAlnum(src[j]) {
				j++
			}
			toks = append(toks, Token{Kind: Ident_, Text: src[i:j], Pos: pos})
			adv(j - i)
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for {
				if j >= len(src) || src[j] == '\n' {
					diag.Fail(diag.Errf("E0001", pos, "unterminated string literal"))
				}
				if src[j] == '"' {
					break
				}
				if src[j] == '\\' {
					j++
					if j >= len(src) {
						diag.Fail(diag.Errf("E0001", pos, "unterminated string literal"))
					}
					switch src[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '0':
						sb.WriteByte(0)
					case '\\':
						sb.WriteByte('\\')
					case '"':
						sb.WriteByte('"')
					default:
						diag.Fail(diag.Errf("E0001", pos, "unknown escape \\%c", src[j]))
					}
				} else {
					sb.WriteByte(src[j])
				}
				j++
			}
			toks = append(toks, Token{Kind: Str_, Text: sb.String(), Pos: pos})
			adv(j + 1 - i)
		default:
			matched := ""
			for _, o := range ops3 {
				if strings.HasPrefix(src[i:], o) {
					matched = o
				}
			}
			if matched == "" {
				for _, o := range ops2 {
					if strings.HasPrefix(src[i:], o) {
						matched = o
						break
					}
				}
			}
			if matched == "" && strings.IndexByte(ops1, c) >= 0 {
				matched = string(c)
			}
			if matched == "" {
				diag.Fail(diag.Errf("E0001", pos, "unexpected character %q", c))
			}
			toks = append(toks, Token{Kind: Op, Text: matched, Pos: pos})
			adv(len(matched))
		}
	}
	toks = append(toks, Token{Kind: EOF, Pos: diag.Pos{Line: line, Col: col}})
	return toks
}

func isAlpha(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isAlnum(c byte) bool { return isAlpha(c) || (c >= '0' && c <= '9') }
