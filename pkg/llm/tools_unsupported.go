package llm

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// ToolsUnsupported reports whether err is a terminal HTTP rejection that
// clearly means this provider or model does not accept tools / tool_choice /
// function-calling. Only 400 and 422 are considered.
//
// Classification prefers structured error.code / error.param / error.message
// over raw substring search. A known unrelated code such as
// context_length_exceeded is never treated as a capability rejection, even
// if the message mentions tools. Affirmative "supports tools" wording,
// generic parameter errors, and ambiguous bodies fail closed.
func ToolsUnsupported(err error) bool {
	var he *httpError
	if !errors.As(err, &he) || he == nil {
		return false
	}
	if he.status != 400 && he.status != 422 {
		return false
	}
	return toolsUnsupportedBody(he.msg)
}

func toolsUnsupportedBody(raw string) bool {
	code, param, _, message := parseProviderError(raw)
	if isUnrelatedErrorCode(code) {
		return false
	}
	if isToolsCapabilityParam(param) && isUnknownToolsParameterCode(code) {
		return true
	}
	text := message
	if strings.TrimSpace(text) == "" {
		text = raw
	}
	return toolsCapabilityDenied(text) || unknownToolsParameter(text)
}

func parseProviderError(raw string) (code, param, typ, message string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", "", ""
	}
	var root any
	if err := json.Unmarshal([]byte(raw), &root); err != nil {
		return "", "", "", raw
	}
	m, ok := root.(map[string]any)
	if !ok {
		return "", "", "", raw
	}

	if ev, ok := m["error"]; ok {
		switch e := ev.(type) {
		case string:
			if strings.TrimSpace(e) != "" {
				message = e
			}
		case map[string]any:
			code = anyString(e["code"])
			param = anyString(e["param"])
			typ = anyString(e["type"])
			if msg := anyString(e["message"]); msg != "" {
				message = msg
			}
		default:
			// Ignore unexpected typed payloads and fall back to top-level fields.
		}
	}
	if code == "" {
		code = anyString(m["code"])
	}
	if param == "" {
		param = anyString(m["param"])
	}
	if typ == "" {
		typ = anyString(m["type"])
	}
	if message == "" {
		if msg := anyString(m["message"]); msg != "" {
			message = msg
		} else {
			message = raw
		}
	}
	return code, param, typ, message
}

func anyString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return strings.TrimSpace(x.String())
	case float64:
		// JSON numbers decode as float64; error codes are usually strings.
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(jsonNumber(x)), ".0"))
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		s := strings.Trim(string(b), `"`)
		if s == "null" {
			return ""
		}
		return strings.TrimSpace(s)
	}
}

func jsonNumber(f float64) string {
	b, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	return string(b)
}

func isUnrelatedErrorCode(code string) bool {
	c := normalizeErrorText(code)
	if c == "" {
		return false
	}
	switch c {
	case "context_length_exceeded", "context_length", "max_tokens", "max_tokens_exceeded",
		"string_above_max_length", "token_limit_exceeded", "prompt_too_long",
		"insufficient_quota", "invalid_api_key", "model_not_found",
		"content_filter", "content_policy_violation", "billing_not_active":
		return true
	}
	if strings.Contains(c, "context_length") || strings.HasPrefix(c, "rate_limit") {
		return true
	}
	return false
}

func isToolsCapabilityParam(param string) bool {
	switch normalizeErrorText(param) {
	case "tools", "tool", "tool_choice", "tool choice", "functions", "function", "function_call", "function call":
		return true
	default:
		return false
	}
}

func isUnknownToolsParameterCode(code string) bool {
	switch normalizeErrorText(code) {
	case "unknown_parameter", "unsupported_parameter", "unknown_field",
		"unexpected_parameter", "unknown_argument", "unsupported_argument":
		return true
	default:
		return false
	}
}

func normalizeErrorText(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "’", "'")
	s = strings.ReplaceAll(s, "‘", "'")
	s = strings.ReplaceAll(s, "`", "'")
	return s
}

var (
	reToolsCapabilityDenied = []*regexp.Regexp{
		regexp.MustCompile(`\b(does\s+not|doesn't|doesnt|do\s+not|don't|dont)\s+support\s+(tool|function)`),
		regexp.MustCompile(`\b(function\s+calling|tool\s+use|tool\s+calling|tool_choice|tool\s+choice|tools?)\s+(is|are)\s+not\s+supported`),
		regexp.MustCompile(`\b(function\s+calling|tool\s+use|tool\s+calling|tool_choice|tool\s+choice|tools?)\s+(isn't|isnt|aren't|arent)\s+supported`),
		regexp.MustCompile(`\bno\s+support\s+for\s+tool`),
		regexp.MustCompile(`\btools?\s+parameter\s+is\s+not\s+supported`),
	}
	reUnknownToolsParameter = []*regexp.Regexp{
		regexp.MustCompile(`\b(unknown|unrecognized)\s+(parameter|field|request\s+argument)s?\s*[: ]\s*['"]?(tools?|tool_choice|functions?|function_call)`),
		regexp.MustCompile(`['"](tools?|tool_choice|functions?|function_call)['"]\s+is\s+not\s+a\s+valid`),
		regexp.MustCompile(`\b(tools?|tool_choice)\s+is\s+not\s+a\s+valid\s+parameter`),
		regexp.MustCompile(`\bunrecognized request argument supplied:\s*['"]?(tools?|tool_choice)`),
	}
)

func toolsCapabilityDenied(message string) bool {
	s := normalizeErrorText(message)
	if s == "" {
		return false
	}
	for _, re := range reToolsCapabilityDenied {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func unknownToolsParameter(message string) bool {
	s := normalizeErrorText(message)
	if s == "" {
		return false
	}
	for _, re := range reUnknownToolsParameter {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
