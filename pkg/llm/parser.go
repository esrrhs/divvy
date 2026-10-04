package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// ExtractJSON finds the first complete JSON object or array in s.
// It understands markdown fences, leading prose, and string-aware brace matching.
func ExtractJSON(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty model output")
	}

	if strings.HasPrefix(s, "```") {
		s = stripFence(s)
	}

	start := -1
	for i, r := range s {
		if r == '{' || r == '[' {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("no JSON object found in model output")
	}

	raw, err := matchJSONBalanced(s[start:])
	if err != nil {
		closed := closeTruncatedJSON(s[start:])
		if closed == "" {
			return "", err
		}
		if _, err2 := matchJSONBalanced(closed); err2 != nil {
			return "", err
		}
		raw = closed
	}
	return repairCommon(raw), nil
}

// UnmarshalFlexible extracts JSON from messy model output and unmarshals it.
func UnmarshalFlexible(s string, dest any) error {
	raw, err := ExtractJSON(s)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	if err := dec.Decode(dest); err != nil {
		return fmt.Errorf("invalid JSON: %w\nsnippet: %s", err, truncate(raw, 400))
	}
	return nil
}

// Action is a normalized worker tool call parsed from weak-model JSON.
type Action struct {
	Thought string
	Name    string
	Args    map[string]any
}

// ParseAction accepts several common weak-model JSON shapes.
func ParseAction(content string) (*Action, error) {
	var generic map[string]any
	if err := UnmarshalFlexible(content, &generic); err != nil {
		return nil, err
	}

	action := &Action{Args: map[string]any{}}
	action.Thought = firstString(generic, "thought", "reasoning", "reason", "scratchpad")
	action.Name = firstString(generic, "action", "tool", "name", "function", "tool_name")

	if nested, ok := generic["function"].(map[string]any); ok && action.Name == "" {
		action.Name = firstString(nested, "name")
		if len(action.Args) == 0 {
			action.Args = asObject(nested["arguments"])
			if len(action.Args) == 0 {
				action.Args = asObject(nested["parameters"])
			}
		}
	}

	for _, key := range []string{"args", "arguments", "parameters", "input", "params"} {
		if obj := asObject(generic[key]); len(obj) > 0 {
			action.Args = obj
			break
		}
	}

	action.Name = strings.TrimSpace(action.Name)
	action.Name = strings.ToLower(action.Name)
	action.Name = strings.ReplaceAll(action.Name, " ", "_")

	switch action.Name {
	case "done", "complete", "completed", "task_complete", "stop", "end":
		action.Name = "finish"
	}

	if action.Name == "" {
		return nil, fmt.Errorf("JSON is missing action/tool name")
	}
	if action.Args == nil {
		action.Args = map[string]any{}
	}
	return action, nil
}

func asObject(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return t
	case string:
		var m map[string]any
		if err := UnmarshalFlexible(t, &m); err == nil {
			return m
		}
	}
	return nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if t, ok := v.(string); ok {
				if strings.TrimSpace(t) != "" {
					return strings.TrimSpace(t)
				}
			}
		}
	}
	return ""
}

func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		lang := strings.TrimSpace(s[:i])
		if lang == "" || isIdent(lang) {
			s = s[i+1:]
		}
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '+' && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func matchJSONBalanced(s string) (string, error) {
	var stack []byte
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, c)
		case '}':
			if len(stack) == 0 || stack[len(stack)-1] != '{' {
				return "", fmt.Errorf("unbalanced JSON braces")
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return s[:i+1], nil
			}
		case ']':
			if len(stack) == 0 || stack[len(stack)-1] != '[' {
				return "", fmt.Errorf("unbalanced JSON brackets")
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return s[:i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unterminated JSON object")
}

// closeTruncatedJSON appends quotes/braces so a cut-off object can still unmarshal.
func closeTruncatedJSON(s string) string {
	if s == "" {
		return ""
	}
	var stack []byte
	inString := false
	escaped := false
	stringStart := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
				stringStart = -1
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			stringStart = i
		case '{', '[':
			stack = append(stack, c)
		case '}':
			if len(stack) > 0 && stack[len(stack)-1] == '{' {
				stack = stack[:len(stack)-1]
			}
		case ']':
			if len(stack) > 0 && stack[len(stack)-1] == '[' {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if !inString && len(stack) == 0 {
		return ""
	}
	if escaped && strings.HasSuffix(s, `\`) {
		s = s[:len(s)-1]
	}
	if inString && stringStart >= 0 {
		prev := lastNonSpace(s[:stringStart])
		if prev == ':' {
			s += `"`
		} else {
			s = strings.TrimRight(s[:stringStart], " \t\n\r,")
		}
	}
	s = strings.TrimRight(s, " \t\n\r,")
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			s += "}"
		} else {
			s += "]"
		}
	}
	return s
}

func repairCommon(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			b.WriteByte(c)
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			b.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\n' || s[j] == '\r' || s[j] == '\t') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func lastNonSpace(s string) byte {
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return c
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
