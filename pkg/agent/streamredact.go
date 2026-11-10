package agent

import "strings"

// streamRedactor emits user-visible text while holding back suffixes that
// could be an incomplete secret, so a key split across chunks never appears
// in a published delta or mid-stream snapshot. Complete secrets are replaced
// with the standard mask before anything is released.
type streamRedactor struct {
	secrets []string
	raw     string
	emitted string
}

func newStreamRedactor(secrets []string) *streamRedactor {
	out := make([]string, 0, len(secrets))
	for _, s := range secrets {
		s = strings.TrimSpace(s)
		if len(s) >= 4 {
			out = append(out, s)
		}
	}
	return &streamRedactor{secrets: out}
}

func (r *streamRedactor) Add(delta string) string {
	if r == nil {
		return delta
	}
	if delta != "" {
		r.raw += delta
	}
	return r.release(false)
}

func (r *streamRedactor) Flush() string {
	if r == nil {
		return ""
	}
	return r.release(true)
}

// Visible is the already-released, redacted text (never includes a held
// mid-stream secret prefix, and never includes a complete secret).
func (r *streamRedactor) Visible() string {
	if r == nil {
		return ""
	}
	return r.emitted
}

func (r *streamRedactor) release(flush bool) string {
	full := r.raw
	hold := 0
	if !flush {
		hold = secretPrefixHold(full, r.secrets)
	}
	safe := full
	if hold > 0 && hold <= len(full) {
		safe = full[:len(full)-hold]
	}
	redacted := applySecrets(safe, r.secrets)
	if !strings.HasPrefix(redacted, r.emitted) {
		// A secret completed inside previously-safe text: replace the
		// published view with the fully redacted form and emit nothing
		// further that could reconstruct the key from earlier chunks
		// plus this delta. Callers that keep a snapshot should use
		// Visible() after Add/Flush.
		r.emitted = redacted
		return ""
	}
	delta := redacted[len(r.emitted):]
	r.emitted = redacted
	return delta
}

func applySecrets(s string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" && strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, secretMask)
		}
	}
	return s
}

// secretPrefixHold returns how many trailing bytes must be withheld because
// they form a prefix (at least 3 chars) of a registered secret. Shorter
// prefixes are released so ordinary sentences are not delayed one letter at
// a time.
func secretPrefixHold(s string, secrets []string) int {
	hold := 0
	for _, sec := range secrets {
		max := len(sec) - 1
		if max < 3 {
			continue
		}
		if len(s) < 3 {
			if strings.HasPrefix(sec, s) && len(s) > hold {
				hold = len(s)
			}
			continue
		}
		upper := max
		if upper > len(s) {
			upper = len(s)
		}
		for k := upper; k >= 3; k-- {
			if strings.HasSuffix(s, sec[:k]) {
				if k > hold {
					hold = k
				}
				break
			}
		}
	}
	return hold
}
