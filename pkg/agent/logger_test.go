package agent

import (
	"bytes"
	"strings"
	"testing"
)

func TestLogger_TeeWritesTimestampedPlainLines(t *testing.T) {
	var buf bytes.Buffer
	l := SilentLogger() // screen output discarded, color off, non-verbose
	l.Tee(&buf)

	l.Infof("hello %s", "world")
	l.Errorf("boom: %d", 42)

	out := buf.String()
	if !strings.Contains(out, "INFO") || !strings.Contains(out, "hello world") {
		t.Fatalf("missing info line: %q", out)
	}
	if !strings.Contains(out, "ERROR") || !strings.Contains(out, "boom: 42") {
		t.Fatalf("missing error line: %q", out)
	}
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		// Expect "2006-01-02 15:04:05.000 LEVEL ..."
		if len(ln) < 23 || ln[4] != '-' || ln[7] != '-' || ln[10] != ' ' {
			t.Fatalf("line lacks timestamp prefix: %q", ln)
		}
		if strings.Contains(ln, "\x1b[") {
			t.Fatalf("persisted line must be plain text, got ANSI: %q", ln)
		}
	}
}

func TestLogger_TeeDebugGatedByVerbose(t *testing.T) {
	var quiet bytes.Buffer
	lq := SilentLogger()
	lq.Tee(&quiet)
	lq.Debugf("hidden")
	if quiet.Len() != 0 {
		t.Fatalf("debug must be gated when not verbose, got %q", quiet.String())
	}

	var screen, file bytes.Buffer
	lv := &Logger{out: &screen, err: &screen, color: false, verbose: true}
	lv.Tee(&file)
	lv.Debugf("shown")
	if !strings.Contains(file.String(), "DEBUG") || !strings.Contains(file.String(), "shown") {
		t.Fatalf("verbose debug missing from log file: %q", file.String())
	}
}

func TestLogger_PrintTeesPlainTree(t *testing.T) {
	var buf bytes.Buffer
	l := SilentLogger()
	l.Tee(&buf)
	l.Print("root\n└── leaf\n")
	out := buf.String()
	if !strings.Contains(out, "└── leaf") {
		t.Fatalf("tree text missing: %q", out)
	}
}
