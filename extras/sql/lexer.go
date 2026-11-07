// Package sql turns a command line into a structured command.
// lexer.go splits text into tokens; parser.go builds commands from them.
package sql

import (
	"fmt"
	"strings"
)

// TokenKind says what a token is.
type TokenKind int

const (
	TokEOF    TokenKind = iota
	TokIdent            // names and keywords: users, SELECT, age
	TokInt              // 42, -7
	TokFloat            // 3.14, -0.5
	TokString           // 'Asha' (Text holds the unquoted value)
	TokSymbol           // ( ) , ; * . = != < > <= >=
)

// Token is one lexical unit.
type Token struct {
	Kind TokenKind
	Text string
	Pos  int // byte offset in the input, for error messages
}

// IsKeyword reports whether t is the identifier kw (case-insensitive).
func (t Token) IsKeyword(kw string) bool {
	return t.Kind == TokIdent && strings.EqualFold(t.Text, kw)
}

// IsSymbol reports whether t is the symbol sym.
func (t Token) IsSymbol(sym string) bool {
	return t.Kind == TokSymbol && t.Text == sym
}

// Describe returns a short human-readable form for error messages.
func (t Token) Describe() string {
	switch t.Kind {
	case TokEOF:
		return "end of command"
	case TokString:
		return fmt.Sprintf("string '%s'", t.Text)
	default:
		return fmt.Sprintf("%q", t.Text)
	}
}

func isLetter(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// Lex splits input into tokens. The result always ends with a TokEOF token.
//
// Rules:
//   - Strings use single quotes; write ” for a quote inside: 'it”s'.
//   - "<>" is normalised to "!=".
//   - A '-' directly before a digit is part of the number: -5, -2.5.
//   - "--" starts a comment that runs to the end of the line.
func Lex(input string) ([]Token, error) {
	var toks []Token
	n := len(input)
	i := 0

	for i < n {
		c := input[i]

		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++

		case c == '-' && i+1 < n && input[i+1] == '-':
			for i < n && input[i] != '\n' {
				i++
			}

		case isLetter(c):
			start := i
			for i < n && (isLetter(input[i]) || isDigit(input[i])) {
				i++
			}
			toks = append(toks, Token{TokIdent, input[start:i], start})

		case isDigit(c) || (c == '-' && i+1 < n && isDigit(input[i+1])):
			start := i
			if c == '-' {
				i++
			}
			for i < n && isDigit(input[i]) {
				i++
			}
			kind := TokInt
			if i+1 < n && input[i] == '.' && isDigit(input[i+1]) {
				kind = TokFloat
				i++
				for i < n && isDigit(input[i]) {
					i++
				}
			}
			if i < n && isLetter(input[i]) {
				return nil, fmt.Errorf("invalid number at position %d", start)
			}
			toks = append(toks, Token{kind, input[start:i], start})

		case c == '\'':
			start := i
			i++
			var sb strings.Builder
			closed := false
			for i < n {
				if input[i] == '\'' {
					if i+1 < n && input[i+1] == '\'' { // escaped quote
						sb.WriteByte('\'')
						i += 2
						continue
					}
					closed = true
					i++
					break
				}
				sb.WriteByte(input[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string starting at position %d", start)
			}
			toks = append(toks, Token{TokString, sb.String(), start})

		default:
			start := i
			switch c {
			case '(', ')', ',', ';', '*', '.', '=':
				i++
				toks = append(toks, Token{TokSymbol, string(c), start})
			case '<':
				switch {
				case i+1 < n && input[i+1] == '=':
					i += 2
					toks = append(toks, Token{TokSymbol, "<=", start})
				case i+1 < n && input[i+1] == '>':
					i += 2
					toks = append(toks, Token{TokSymbol, "!=", start})
				default:
					i++
					toks = append(toks, Token{TokSymbol, "<", start})
				}
			case '>':
				if i+1 < n && input[i+1] == '=' {
					i += 2
					toks = append(toks, Token{TokSymbol, ">=", start})
				} else {
					i++
					toks = append(toks, Token{TokSymbol, ">", start})
				}
			case '!':
				if i+1 < n && input[i+1] == '=' {
					i += 2
					toks = append(toks, Token{TokSymbol, "!=", start})
				} else {
					return nil, fmt.Errorf("unexpected '!' at position %d (did you mean !=?)", start)
				}
			default:
				return nil, fmt.Errorf("unexpected character %q at position %d", string(c), start)
			}
		}
	}

	toks = append(toks, Token{TokEOF, "", n})
	return toks, nil
}
