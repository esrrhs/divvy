package tools

import (
	"os"
	"path/filepath"
)

// ProjectType identifies the toolchain a workspace is built around, so the
// verifier can generate language-appropriate acceptance commands instead of
// assuming Go.
type ProjectType string

const (
	ProjectGeneric ProjectType = "generic"
	ProjectGo      ProjectType = "go"
	ProjectNode    ProjectType = "node"
	ProjectRust    ProjectType = "rust"
	ProjectPython  ProjectType = "python"
	ProjectMake    ProjectType = "make"
)

// DetectProject identifies the workspace's primary toolchain from marker
// files. When several coexist, the most specific marker wins in a fixed
// order; absence of every marker is ProjectGeneric.
func DetectProject(root string) ProjectType {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}
	switch {
	case exists("go.mod"):
		return ProjectGo
	case exists("package.json"):
		return ProjectNode
	case exists("Cargo.toml"):
		return ProjectRust
	case exists("pyproject.toml") || exists("requirements.txt") || exists("setup.py"):
		return ProjectPython
	case exists("Makefile") || exists("makefile"):
		return ProjectMake
	default:
		return ProjectGeneric
	}
}
