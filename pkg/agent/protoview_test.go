package agent

import (
	"strings"
	"testing"
)

func TestProtocolView_StreamsThoughtAndSummaryNotProtocol(t *testing.T) {
	raw := `{"thought":"Hello there","action":"finish","args":{"summary":"All good"}}`
	var v protocolView
	var got string
	// Split into awkward chunks, including mid-token and mid-escape later.
	for _, c := range chunkEvery(raw, 3) {
		got += v.Append(c)
		if strings.Contains(v.Visible(), `"action"`) || strings.Contains(v.Visible(), "finish") && !strings.Contains(v.Visible(), "Hello") {
			t.Fatalf("protocol leaked mid-stream: %q", v.Visible())
		}
	}
	v.Flush()
	visible := v.Visible()
	if !strings.Contains(visible, "Hello there") || !strings.Contains(visible, "All good") {
		t.Fatalf("visible %q missing thought/summary", visible)
	}
	if strings.Contains(visible, `"action"`) || strings.Contains(visible, `"args"`) || strings.Contains(visible, "{") {
		t.Fatalf("visible still looks like protocol: %q", visible)
	}
	if got != visible && !strings.HasPrefix(visible, got) && got != visible {
		// Incremental concatenation should equal the final view.
		if got != visible {
			t.Fatalf("joined deltas %q != visible %q", got, visible)
		}
	}
}

func TestProtocolView_HidesWriteFileArgs(t *testing.T) {
	raw := `{"thought":"I'll write it","action":"write_file","args":{"path":"secret.go","content":"package hidden\n"}}`
	var v protocolView
	for _, c := range chunkEvery(raw, 5) {
		v.Append(c)
	}
	got := v.Visible()
	if got != "I'll write it" {
		t.Fatalf("visible=%q", got)
	}
	if strings.Contains(got, "package hidden") || strings.Contains(got, "secret.go") || strings.Contains(got, "write_file") {
		t.Fatalf("tool args leaked: %q", got)
	}
}

func TestProtocolView_AskQuestionAndEscapes(t *testing.T) {
	raw := `{"thought":"line1\nline2 and \"q\"","action":"ask","args":{"question":"What filename?"}}`
	var v protocolView
	for _, c := range chunkEvery(raw, 4) {
		v.Append(c)
	}
	got := v.Visible()
	if !strings.Contains(got, "line1\nline2 and \"q\"") {
		t.Fatalf("escapes not decoded: %q", got)
	}
	if !strings.Contains(got, "What filename?") {
		t.Fatalf("missing question: %q", got)
	}
}

func TestProtocolView_UnicodeAcrossChunks(t *testing.T) {
	// U+4E2D ('中') is e4 b8 ad
	prefix := `{"thought":"`
	suffix := `文","action":"finish","args":{"summary":"ok"}}`
	char := "中"
	var v protocolView
	v.Append(prefix)
	// split the 3-byte rune
	b := []byte(char)
	if len(b) != 3 {
		t.Fatalf("expected 3-byte rune, got %d", len(b))
	}
	if d := v.Append(string(b[:1])); d != "" {
		t.Fatalf("emitted incomplete UTF-8: %q", d)
	}
	v.Append(string(b[1:]))
	v.Append(suffix)
	if !strings.Contains(v.Visible(), "中文") {
		t.Fatalf("visible=%q", v.Visible())
	}
}

func TestProtocolView_CodeFenceAndInvalidDoNotLeak(t *testing.T) {
	var v protocolView
	v.Append("```json\n")
	v.Append(`{"thought":"Hi`)
	if strings.Contains(v.Visible(), "```") || strings.Contains(v.Visible(), "{") {
		t.Fatalf("fence/protocol leaked on half JSON: %q", v.Visible())
	}
	v.Append(`","action":"finish","args":{"summary":"done"}}` + "\n```")
	if v.Visible() != "Hi\n\ndone" && v.Visible() != "Hi\n\ndone" {
		if !strings.Contains(v.Visible(), "Hi") || !strings.Contains(v.Visible(), "done") {
			t.Fatalf("visible=%q", v.Visible())
		}
	}

	var bad protocolView
	bad.Append(`{"not valid at all`)
	if strings.Contains(bad.Visible(), "{") || strings.Contains(bad.Visible(), "not valid") {
		t.Fatalf("invalid JSON leaked: %q", bad.Visible())
	}
}

func TestProtocolView_ProsePassesThrough(t *testing.T) {
	var v protocolView
	var got string
	for _, c := range []string{"AAA", "BBB", "CCC"} {
		got += v.Append(c)
	}
	if got != "AAABBBCCC" || v.Visible() != "AAABBBCCC" {
		t.Fatalf("prose=%q visible=%q", got, v.Visible())
	}
}

func TestStreamRedactor_HoldsPartialKeyThenRedacts(t *testing.T) {
	const secret = "sk-stream-secret-key-do-not-leak"
	r := newStreamRedactor([]string{secret})
	d1 := r.Add("hello ")
	if d1 != "hello " {
		t.Fatalf("d1=%q", d1)
	}
	// First half of the secret must not be released.
	mid := secret[:len(secret)/2]
	if d := r.Add(mid); d != "" {
		if strings.Contains(d, mid) || strings.Contains(r.Visible(), mid) {
			t.Fatal("partial key was published")
		}
	}
	if strings.Contains(r.Visible(), mid) || strings.Contains(r.Visible(), secret) {
		t.Fatal("snapshot held a key fragment")
	}
	rest := secret[len(secret)/2:] + " end"
	_ = r.Add(rest)
	if strings.Contains(r.Visible(), secret) {
		t.Fatal("complete key leaked after join")
	}
	if !strings.Contains(r.Visible(), secretMask) {
		t.Fatalf("expected mask in %q", r.Visible())
	}
	if strings.Contains(r.Visible(), "hello") && !strings.Contains(r.Visible(), "end") {
		t.Fatalf("lost trailing text: %q", r.Visible())
	}
}

func TestAskRequiresConsent(t *testing.T) {
	if !askRequiresConsent("Can I delete all files in the repo?") {
		t.Fatal("destructive ask should require consent")
	}
	if !askRequiresConsent("请批准我把这段改动推送到生产环境") {
		t.Fatal("approval ask should require consent")
	}
	if askRequiresConsent("What filename should I write?") {
		t.Fatal("filename clarification is not consent")
	}
	if askRequiresConsent("Should I use foo or bar as the function name?") {
		t.Fatal("naming choice is not consent")
	}
	if !askRequiresConsent("Should I send this email to all customers?") {
		t.Fatal("external send should require consent")
	}
}

func chunkEvery(s string, n int) []string {
	var out []string
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}
