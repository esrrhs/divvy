package agent

import (
	"strings"
	"unicode/utf8"
)

// protocolView incrementally turns a model's raw token stream into
// user-visible text. The JSON tool protocol (action/args, including file
// contents and commands) is never shown; thought, finish summaries, and
// ask questions are decoded as they become available, including across
// chunk boundaries, escapes, and incomplete UTF-8.
type protocolView struct {
	raw      string
	pending  []byte // incomplete UTF-8 suffix
	protocol bool
	decided  bool
	visible  string
	lastEmit int
}

func (v *protocolView) Append(chunk string) string {
	if chunk == "" && len(v.pending) == 0 {
		return ""
	}
	buf := append(append([]byte(nil), v.pending...), []byte(chunk)...)
	complete, rest := splitCompleteUTF8(buf)
	v.pending = rest
	if len(complete) == 0 {
		return ""
	}
	v.raw += string(complete)
	v.refresh()
	if len(v.visible) < v.lastEmit {
		v.lastEmit = len(v.visible)
		return ""
	}
	delta := v.visible[v.lastEmit:]
	v.lastEmit = len(v.visible)
	return delta
}

func (v *protocolView) Visible() string { return v.visible }

func (v *protocolView) Flush() string {
	if len(v.pending) > 0 {
		// Stream ended with an incomplete UTF-8 sequence: drop it rather
		// than emitting replacement runes or raw bytes.
		v.pending = nil
	}
	return v.Append("")
}

func (v *protocolView) refresh() {
	s := v.raw
	if !v.decided {
		t := strings.TrimLeft(s, " \t\r\n")
		if t == "" {
			return
		}
		if strings.HasPrefix(t, "```") {
			v.protocol = true
			v.decided = true
		} else if strings.HasPrefix(t, "{") {
			v.protocol = true
			v.decided = true
		} else {
			v.protocol = false
			v.decided = true
		}
	}
	if !v.protocol {
		v.visible = s
		return
	}
	thought, action, summary, question := scanProtocolFields(s)
	var b strings.Builder
	if thought != "" {
		b.WriteString(thought)
	}
	extra := ""
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "finish", "done", "complete", "completed", "task_complete", "stop", "end", "":
		extra = summary
	case "ask":
		extra = question
	default:
		// Tool calls: thought is the only user-facing narration. Args stay
		// on the tool-activity card.
	}
	if extra != "" && extra != thought {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(extra)
	}
	v.visible = b.String()
}

func splitCompleteUTF8(b []byte) (complete, rest []byte) {
	if len(b) == 0 {
		return nil, nil
	}
	i := 0
	for i < len(b) {
		if !utf8.FullRune(b[i:]) {
			return b[:i], b[i:]
		}
		_, n := utf8.DecodeRune(b[i:])
		if n <= 0 {
			return b[:i], b[i:]
		}
		i += n
	}
	return b, nil
}

func scanProtocolFields(s string) (thought, action, summary, question string) {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "```") {
		if i := strings.IndexByte(t, '\n'); i >= 0 {
			t = t[i+1:]
		} else {
			return "", "", "", ""
		}
		t = strings.TrimPrefix(t, "```")
	}
	start := strings.IndexByte(t, '{')
	if start < 0 {
		return "", "", "", ""
	}
	p := &jsScan{s: t[start:]}
	var f protoFields
	if p.i < len(p.s) && p.s[p.i] == '{' {
		p.i++
		p.parseObject(1, false, &f)
	}
	return f.thought, f.action, f.summary, f.question
}

type protoFields struct {
	thought, action, summary, question string
}

type jsScan struct {
	s string
	i int
}

func (p *jsScan) parseObject(depth int, inArgs bool, out *protoFields) {
	for p.i < len(p.s) {
		p.skipSpace()
		if p.i >= len(p.s) {
			return
		}
		if p.s[p.i] == '}' {
			p.i++
			return
		}
		if p.s[p.i] == ',' {
			p.i++
			continue
		}
		if p.s[p.i] != '"' {
			return
		}
		key, keyOK := p.readString()
		if !keyOK {
			return
		}
		p.skipSpace()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return
		}
		p.i++
		p.skipSpace()
		if p.i >= len(p.s) {
			return
		}
		switch p.s[p.i] {
		case '"':
			val, _ := p.readString()
			recordProtoField(key, val, depth, inArgs, out)
		case '{':
			p.i++
			nextArgs := inArgs || isArgsKey(key)
			p.parseObject(depth+1, nextArgs, out)
		case '[':
			p.skipArray()
		default:
			p.skipLiteral()
		}
	}
}

func recordProtoField(key, val string, depth int, inArgs bool, out *protoFields) {
	k := strings.ToLower(strings.TrimSpace(key))
	top := depth == 1 && !inArgs
	switch k {
	case "thought", "reasoning", "reason", "scratchpad":
		if top || depth == 1 {
			out.thought = val
		}
	case "action", "tool", "tool_name", "name", "function":
		if top {
			out.action = val
		}
	case "summary":
		if inArgs || top {
			out.summary = val
		}
	case "question":
		if inArgs || top {
			out.question = val
		}
	}
}

func isArgsKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "args", "arguments", "parameters", "input", "params":
		return true
	}
	return false
}

func (p *jsScan) readString() (string, bool) {
	if p.i >= len(p.s) || p.s[p.i] != '"' {
		return "", false
	}
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '"' {
			p.i++
			return b.String(), true
		}
		if c == '\\' {
			if p.i+1 >= len(p.s) {
				return b.String(), false
			}
			p.i++
			esc := p.s[p.i]
			p.i++
			switch esc {
			case '"', '\\', '/':
				b.WriteByte(esc)
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'u':
				if p.i+4 > len(p.s) {
					// Incomplete \uXXXX: do not emit a partial code point.
					return b.String(), false
				}
				r, ok := parseHexRune(p.s[p.i : p.i+4])
				if !ok {
					return b.String(), false
				}
				p.i += 4
				b.WriteRune(r)
			default:
				b.WriteByte(esc)
			}
			continue
		}
		b.WriteByte(c)
		p.i++
	}
	return b.String(), false
}

func parseHexRune(hex string) (rune, bool) {
	if len(hex) != 4 {
		return 0, false
	}
	var r rune
	for i := 0; i < 4; i++ {
		c := hex[i]
		var v rune
		switch {
		case c >= '0' && c <= '9':
			v = rune(c - '0')
		case c >= 'a' && c <= 'f':
			v = rune(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			v = rune(c - 'A' + 10)
		default:
			return 0, false
		}
		r = r<<4 | v
	}
	return r, true
}

func (p *jsScan) skipSpace() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jsScan) skipLiteral() {
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == ',' || c == '}' || c == ']' || c == ' ' || c == '\n' || c == '\t' || c == '\r' {
			return
		}
		p.i++
	}
}

func (p *jsScan) skipArray() {
	if p.i >= len(p.s) || p.s[p.i] != '[' {
		return
	}
	p.i++
	depth := 1
	inStr := false
	esc := false
	for p.i < len(p.s) && depth > 0 {
		c := p.s[p.i]
		p.i++
		if inStr {
			if esc {
				esc = false
				continue
			}
			if c == '\\' {
				esc = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '[':
			depth++
		case ']':
			depth--
		case '{':
			// Skip nested objects inside arrays without recording fields.
			p.i--
			p.skipNestedObject()
		}
	}
}

func (p *jsScan) skipNestedObject() {
	if p.i >= len(p.s) || p.s[p.i] != '{' {
		return
	}
	p.i++
	depth := 1
	inStr := false
	esc := false
	for p.i < len(p.s) && depth > 0 {
		c := p.s[p.i]
		p.i++
		if inStr {
			if esc {
				esc = false
				continue
			}
			if c == '\\' {
				esc = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
		}
	}
}
