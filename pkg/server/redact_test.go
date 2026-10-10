package server

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	secret := "sk-secret-1234567890"
	in := "header auth: " + secret + "\nbody repeats " + secret
	out := Redact(in, secret)
	if strings.Contains(out, secret) {
		t.Fatalf("secret survived redaction: %q", out)
	}
	if !strings.Contains(out, redaction) {
		t.Fatalf("redaction marker missing: %q", out)
	}
}

func TestRedactBytes(t *testing.T) {
	secret := "tok-abcdefghijkl"
	data := []byte(`{"authorization":"` + secret + `"}`)
	out := RedactBytes(data, secret)
	if strings.Contains(string(out), secret) {
		t.Fatalf("secret survived byte redaction: %q", out)
	}
	if string(RedactBytes(data)) != string(data) {
		t.Fatal("redaction with no secrets must not touch payload")
	}
}

func TestRedactIgnoresShortValues(t *testing.T) {
	// A 1-3 char "secret" must not wipe punctuation-like substrings.
	in := "a-b-c"
	if got := Redact(in, "a", "ab", "abc"); got != in {
		t.Fatalf("short values must be ignored, got %q", got)
	}
}
