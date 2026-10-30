package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func layoutKinds(issues []LayoutIssue) string {
	parts := make([]string, 0, len(issues))
	for _, is := range issues {
		parts = append(parts, string(is.Kind)+":"+is.Path)
	}
	return strings.Join(parts, ",")
}

func TestCheckLayout_CleanWorkspaceHasNoIssues(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module app\n\ngo 1.22\n")
	writeFile(t, filepath.Join(root, "cmd", "demo", "main.go"), "package main\n")
	writeFile(t, filepath.Join(root, "internal", "store", "store.go"), "package store\n")
	writeFile(t, filepath.Join(root, "README.md"), "no go files here\n")

	issues, err := CheckLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("clean workspace reported %s", layoutKinds(issues))
	}
}

func TestCheckLayout_FlagsSymlinkAliasOnce(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "internal", "core", "core.go"), "package core\n")
	if err := os.Symlink(filepath.Join("internal", "core"), filepath.Join(root, "core")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	issues, err := CheckLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := layoutKinds(issues); got != "symlink:core" {
		t.Fatalf("issues = %s, want symlink:core", got)
	}
	if !strings.Contains(issues[0].Detail, "internal/core") {
		t.Errorf("detail should name the target, got %q", issues[0].Detail)
	}
	// The link is walked as one entry; the files behind it must not be
	// reported a second time under the alias path.
	if strings.Contains(layoutKinds(issues), "core/core.go") {
		t.Errorf("walk descended through the link: %s", layoutKinds(issues))
	}
}

func TestCheckLayout_FlagsLinkOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "shared.go"), "package shared\n")
	if err := os.Symlink(outside, filepath.Join(root, "shared")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	issues, err := CheckLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Kind != LayoutLinkAlias {
		t.Fatalf("issues = %s", layoutKinds(issues))
	}
	if !strings.Contains(issues[0].Detail, "outside the workspace") {
		t.Errorf("detail = %q", issues[0].Detail)
	}
}

func TestCheckLayout_FlagsDanglingLink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(filepath.Join("gone", "away"), filepath.Join(root, "stale")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	issues, err := CheckLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || !strings.Contains(issues[0].Detail, "dangling") {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestCheckLayout_SkipsIgnoredDirectories(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "node_modules", "pkg", "index.js"), "x\n")
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..", filepath.Join(root, "node_modules", "loop")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	issues, err := CheckLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("ignored directory reported: %s", layoutKinds(issues))
	}
}

func TestCheckLayout_FlagsModuleNesting(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module app\n")
	// The mismatch: module "app" means app/store is ./store, but the sources
	// live under ./app/store, i.e. app/app/store.
	writeFile(t, filepath.Join(root, "app", "store", "store.go"), "package store\n")
	writeFile(t, filepath.Join(root, "app", "store", "store_test.go"), "package store\n")

	issues, err := CheckLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := layoutKinds(issues); got != "module-nesting:app" {
		t.Fatalf("issues = %s, want module-nesting:app", got)
	}
	if !strings.Contains(issues[0].Detail, `"app"`) || !strings.Contains(issues[0].Detail, "2 Go file(s)") {
		t.Errorf("detail = %q", issues[0].Detail)
	}
}

func TestCheckLayout_FlagsFullModulePathDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module github.com/org/app\n")
	writeFile(t, filepath.Join(root, "github.com", "org", "app", "svc", "svc.go"), "package svc\n")

	issues, err := CheckLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := layoutKinds(issues); got != "module-nesting:github.com/org/app" {
		t.Fatalf("issues = %s", got)
	}
}

func TestCheckLayout_IgnoresNestedModule(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module app\n")
	// A monorepo subproject legitimately owns the "app" directory: its own
	// go.mod defines the import paths under it.
	writeFile(t, filepath.Join(root, "app", "go.mod"), "module example.com/app\n")
	writeFile(t, filepath.Join(root, "app", "svc.go"), "package app\n")

	issues, err := CheckLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("nested module reported: %s", layoutKinds(issues))
	}
}

func TestCheckLayout_IgnoresModuleDirWithoutGoFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module app\n")
	// A same-named directory holding only assets is harmless: no package
	// import can name it.
	writeFile(t, filepath.Join(root, "app", "logo.svg"), "<svg/>\n")

	got, err := CheckLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("asset-only directory reported: %s", layoutKinds(got))
	}
}

func TestCheckLayout_NoGoModMeansNoNestingCheck(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "app", "main.go"), "package main\n")
	issues, err := CheckLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues without go.mod: %s", layoutKinds(issues))
	}
}

func TestGoModulePath(t *testing.T) {
	cases := []struct {
		name  string
		goMod string
		want  string
	}{
		{"plain", "module app\n\ngo 1.22\n", "app"},
		{"quoted", "module \"github.com/x/y\"\n", "github.com/x/y"},
		{"leading comment", "// module notme\nmodule real/one\n", "real/one"},
		{"missing", "", ""},
		{"no directive", "go 1.22\nrequire x v1.0.0\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "go.mod"), c.goMod)
			if got := goModulePath(root); got != c.want {
				t.Fatalf("goModulePath = %q, want %q", got, c.want)
			}
		})
	}
}

// TestCheckLayout_RealRepoIsClean runs the check against this repository: a
// preflight that flags the orchestrator's own tree would fire on every run.
func TestCheckLayout_RealRepoIsClean(t *testing.T) {
	issues, err := CheckLayout("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range issues {
		if is.Kind == LayoutModuleNesting {
			t.Errorf("false positive on the divvy repo: %s: %s", is.Path, is.Detail)
		}
	}
}

func TestCheckLeafLayout_FlagsModuleNestingAfterSoftPass(t *testing.T) {
	// Regression for the FreeLLM physics failure: soft grep DoD passed against
	// physics2d/physics/x.go while go.mod said module physics2d. The leaf must
	// fail layout before merge, not leave later leaves to rediscover the drift.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module physics2d\n\ngo 1.22\n")
	writeFile(t, filepath.Join(root, "physics2d", "physics", "physics.go"), "package physics\n")

	issues, err := CheckLeafLayout(root, []string{"physics2d/physics/physics.go"})
	if err != nil {
		t.Fatal(err)
	}
	kinds := layoutKinds(issues)
	if !strings.Contains(kinds, "module-nesting") && !strings.Contains(kinds, "contract-drift") {
		t.Fatalf("issues = %s, want module-nesting or contract-drift", kinds)
	}
	msg := FormatLayoutIssues(issues)
	if !strings.Contains(msg, "layout drift") {
		t.Fatalf("FormatLayoutIssues = %q", msg)
	}
	if !strings.Contains(msg, "physics2d") {
		t.Fatalf("message should name the drifted path: %q", msg)
	}
}

func TestCheckLeafLayout_FlagsFoundVsExpected(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module physics2d\n")
	writeFile(t, filepath.Join(root, "physics2d", "physics", "physics.go"), "package physics\n")

	issues, err := CheckLeafLayout(root, []string{"physics/physics.go"})
	if err != nil {
		t.Fatal(err)
	}
	var drift *LayoutIssue
	for i := range issues {
		if issues[i].Kind == LayoutContractDrift && issues[i].Path == "physics/physics.go" {
			drift = &issues[i]
			break
		}
	}
	if drift == nil {
		t.Fatalf("missing contract-drift for physics/physics.go in %s", layoutKinds(issues))
	}
	if !strings.Contains(drift.Detail, "physics2d/physics/physics.go") {
		t.Fatalf("detail should name found path, got %q", drift.Detail)
	}
}

func TestCheckLeafLayout_CleanContractPasses(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module physics2d\n")
	writeFile(t, filepath.Join(root, "physics", "physics.go"), "package physics\n")

	issues, err := CheckLeafLayout(root, []string{"physics/physics.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("clean layout reported %s", layoutKinds(issues))
	}
}

func TestCheckLeafLayout_FlagsExtraGoMod(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module app\n")
	writeFile(t, filepath.Join(root, "pkg", "go.mod"), "module app/pkg\n")
	writeFile(t, filepath.Join(root, "pkg", "x.go"), "package pkg\n")

	issues, err := CheckLeafLayout(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(layoutKinds(issues), "extra-go-mod:pkg/go.mod") {
		t.Fatalf("issues = %s, want extra-go-mod:pkg/go.mod", layoutKinds(issues))
	}
}

func TestContractNestingOutputs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module physics2d\n")
	issues := ContractNestingOutputs(root, []string{"physics2d/physics/x.go", "physics/x.go"})
	if len(issues) != 1 || issues[0].Path != "physics2d/physics/x.go" {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestFormatLayoutIssues_Empty(t *testing.T) {
	if FormatLayoutIssues(nil) != "" {
		t.Fatal("empty issues should format to empty string")
	}
}
