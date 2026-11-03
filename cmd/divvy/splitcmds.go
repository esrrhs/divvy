package main

import "strings"

// splitCommandList splits an operator-supplied -dod / -aggregate value on
// top-level ';' separators. Semicolons inside single/double quotes, or after
// an unquoted shell background '&' that is part of a `(cmd & …; …)` smoke
// script, stay inside the same command — blind strings.Split broke legitimate
// acceptance like:
//
//	go build -o build/x . && (build/x & SRV=$!; sleep 1; curl -sf …; kill $SRV)
//
// Quote-aware splitting is enough for the documented "several commands in one
// flag, separated by ';'" contract without requiring the operator to escape
// every smoke-test semicolon.
func splitCommandList(v string) []string {
	var out []string
	var b strings.Builder
	inSingle, inDouble, escaped := false, false, false
	depthParen := 0
	flush := func() {
		s := strings.TrimSpace(b.String())
		b.Reset()
		if s != "" {
			out = append(out, s)
		}
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if escaped {
			b.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' && !inSingle {
			b.WriteByte(c)
			escaped = true
			continue
		}
		if inSingle {
			b.WriteByte(c)
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			b.WriteByte(c)
			if c == '"' {
				inDouble = false
			}
			continue
		}
		switch c {
		case '\'':
			inSingle = true
			b.WriteByte(c)
		case '"':
			inDouble = true
			b.WriteByte(c)
		case '(':
			depthParen++
			b.WriteByte(c)
		case ')':
			if depthParen > 0 {
				depthParen--
			}
			b.WriteByte(c)
		case ';':
			if depthParen > 0 {
				b.WriteByte(c)
				continue
			}
			flush()
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return out
}
