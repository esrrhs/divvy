package tools

import (
	"context"
	"strings"
	"testing"
)

func TestOutline_GoAST(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	src := `package pkg

import "context"

type Mode int

type Widget struct {
	x int
}

type Runner interface {
	Run() error
}

const defaultMode = 1

var a, b int

func Build(m Mode) (*Widget, error) { return nil, nil }

func (w *Widget) Run(ctx context.Context) error { return nil }
`
	if err := sb.WriteFile("pkg/x.go", src); err != nil {
		t.Fatal(err)
	}
	out, err := sb.Call(context.Background(), ToolOutline, map[string]any{"path": "pkg/x.go"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"type Mode int",
		"type Widget struct",
		"type Runner interface",
		"const defaultMode",
		"var a, b",
		"func Build(m Mode) (*Widget, error)",
		"method func (w *Widget) Run(ctx context.Context) error",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("outline missing %q:\n%s", want, out)
		}
	}
	// Bodies must not leak into the outline.
	if strings.Contains(out, "return nil") || strings.Contains(out, "x int") {
		t.Fatalf("outline must omit struct/function bodies:\n%s", out)
	}
}

func TestOutline_PatternFallback(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("svc.py", `class Service:
    def __init__(self):
        pass

async def fetch(self):
    return 1

def helper():
    pass
`); err != nil {
		t.Fatal(err)
	}
	out, err := sb.Call(context.Background(), ToolOutline, map[string]any{"path": "svc.py"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type class Service", "func def __init__", "func async def fetch", "func def helper"} {
		if !strings.Contains(out, want) {
			t.Fatalf("python outline missing %q:\n%s", want, out)
		}
	}
	// Indented methods appear at their real line numbers.
	if !strings.Contains(out, "2:") || !strings.Contains(out, "5:") || !strings.Contains(out, "8:") {
		t.Fatalf("line numbers wrong:\n%s", out)
	}
}

func TestOutline_EmptyAndMissing(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	if err := sb.WriteFile("notes.txt", "just prose, no declarations\n"); err != nil {
		t.Fatal(err)
	}
	out, err := sb.Call(context.Background(), ToolOutline, map[string]any{"path": "notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no top-level declarations") {
		t.Fatalf("expected empty-outline marker, got:\n%s", out)
	}
	if _, err := sb.Call(context.Background(), ToolOutline, map[string]any{"path": "missing.go"}); err == nil {
		t.Fatal("missing file must fail")
	}
}
