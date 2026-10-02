package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectProject(t *testing.T) {
	cases := []struct {
		name   string
		marker string
		want   ProjectType
	}{
		{"go", "go.mod", ProjectGo},
		{"node", "package.json", ProjectNode},
		{"rust", "Cargo.toml", ProjectRust},
		{"python pyproject", "pyproject.toml", ProjectPython},
		{"python requirements", "requirements.txt", ProjectPython},
		{"python setup", "setup.py", ProjectPython},
		{"make", "Makefile", ProjectMake},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.marker), []byte("x"), 0644); err != nil {
				t.Fatal(err)
			}
			if got := DetectProject(dir); got != c.want {
				t.Fatalf("DetectProject() = %q, want %q", got, c.want)
			}
		})
	}

	t.Run("generic when empty", func(t *testing.T) {
		if got := DetectProject(t.TempDir()); got != ProjectGeneric {
			t.Fatalf("expected generic, got %q", got)
		}
	})

	t.Run("priority when multiple markers", func(t *testing.T) {
		dir := t.TempDir()
		for _, m := range []string{"package.json", "go.mod", "requirements.txt"} {
			if err := os.WriteFile(filepath.Join(dir, m), []byte("x"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		// go.mod wins over both others.
		if got := DetectProject(dir); got != ProjectGo {
			t.Fatalf("expected go to win priority, got %q", got)
		}
	})
}
