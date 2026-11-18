package llm

import (
	"errors"
	"strings"
)

// ContextLengthExceeded reports whether err is a terminal HTTP rejection that
// clearly means the request exceeded the provider's context window. Only 400
// and 422 are considered, and only when a structured error code or the
// message names a context/prompt length limit.
//
// This is not a token-accurate measurement and must not be treated as a
// tools-unsupported rejection. 401, 403, 429, 5xx, cancellations, network
// failures, and generic unrelated 400s fail closed.
func ContextLengthExceeded(err error) bool {
	var he *httpError
	if !errors.As(err, &he) || he == nil {
		return false
	}
	if he.status != 400 && he.status != 422 {
		return false
	}
	if ToolsUnsupported(err) {
		return false
	}
	code, _, _, message := parseProviderError(he.msg)
	if isContextLengthCode(code) {
		return true
	}
	if isContextLengthMessage(message) {
		return true
	}
	if message == "" || message == he.msg {
		return isContextLengthMessage(he.msg)
	}
	return false
}

func isContextLengthCode(code string) bool {
	c := normalizeErrorText(code)
	if c == "" {
		return false
	}
	switch c {
	case "context_length_exceeded", "context_length", "prompt_too_long":
		return true
	}
	return strings.Contains(c, "context_length")
}

func isContextLengthMessage(msg string) bool {
	s := normalizeErrorText(msg)
	if s == "" {
		return false
	}
	for _, n := range []string{
		"maximum context length exceeded",
		"context length exceeded",
		"context_length_exceeded",
		"prompt is too long",
		"prompt too long",
		"please reduce the length of the messages",
		"please reduce the length of the prompt",
		"exceeds the model's context",
		"exceeds the context window",
		"this model's maximum context length is",
		"max context length",
	} {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
