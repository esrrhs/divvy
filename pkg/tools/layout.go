package tools

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// LayoutIssueKind classifies a workspace inconsistency that makes a file path
// mean more than one thing. It is decided from the directory tree alone, so
// it can be reported before any leaf runs and without building the project.
type LayoutIssueKind string

const (
	// LayoutLinkAlias: a path in the workspace is a symlink. Two mechanisms
	// make this a problem for a merge-based orchestrator. A copy mirror does
	// not carry links (copyTree skips non-regular files), so inside a leaf's
	// mirror the alias is an ordinary new directory: work done under it and
	// work done under the real directory are unrelated files that the merge
	// sees as two different paths. And a merge that does travel through the
	// link writes wherever it points, not at the path being merged.
	LayoutLinkAlias LayoutIssueKind = "symlink"

	// LayoutModuleNesting: the tree holds a directory named like the module
	// path (or its last element), so the import path a file claims and the
	// directory it lives in disagree by one level. Every import of the natural
	// path fails to resolve and the fix is a directory move, which a leaf
	// cannot infer from a compiler error it can only partially see.
	LayoutModuleNesting LayoutIssueKind = "module-nesting"
)

// LayoutIssue is one workspace-level inconsistency. Path is workspace-relative
// and slash-separated so it can be compared with mirror and DoD paths.
type LayoutIssue struct {
	Path   string          `json:"path"`
	Kind   LayoutIssueKind `json:"kind"`
	Detail string          `json:"detail"`
}

// CheckLayout inspects the directory tree under root for inconsistencies that
// will make file-addressed work ambiguous once leaves run in isolated mirrors.
// It reads directory entries and one go.mod; it never reads source files and
// never follows a symlink, so it is cheap enough to run at startup.
//
// An error is returned only when root itself cannot be read: an unreadable
// subdirectory is skipped rather than failing the check, because a preflight
// diagnostic must never be the reason a run stops.
func CheckLayout(root string) ([]LayoutIssue, error) {
	var issues []LayoutIssue
	// goFiles counts .go files per directory (slash-relative). It answers
	// "does this subtree hold Go sources?" without a second walk.
	goFiles := make(map[string]int)

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if p == root {
				return werr
			}
			return nil
		}
		if p == root {
			return nil
		}
		if d.IsDir() {
			if skipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if skipDirNames[d.Name()] {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		// Checked before the regular-file branch: WalkDir reports a symlink as
		// a non-directory entry, and a link to a directory must not be walked
		// into (that would double-count every file behind it).
		if d.Type()&fs.ModeSymlink != 0 {
			issues = append(issues, linkIssue(root, p, rel))
			return nil
		}
		if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".go") {
			goFiles[path.Dir(rel)]++
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("check workspace layout: %w", walkErr)
	}

	if issue, ok := moduleNestingIssue(root, goFiles); ok {
		issues = append(issues, issue)
	}

	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Kind != issues[j].Kind {
			return issues[i].Kind < issues[j].Kind
		}
		return issues[i].Path < issues[j].Path
	})
	return issues, nil
}

// linkIssue describes where a symlink leads, which is what decides how badly
// it will confuse a merge: a link out of the workspace escapes the mirror, a
// link to a sibling makes one file addressable two ways.
func linkIssue(root, abs, rel string) LayoutIssue {
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return LayoutIssue{Path: rel, Kind: LayoutLinkAlias,
			Detail: "dangling link: nothing resolves here, so files created under this path are unreachable by the name that points at them"}
	}
	inside, rerr := filepath.Rel(root, resolved)
	if rerr != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return LayoutIssue{Path: rel, Kind: LayoutLinkAlias,
			Detail: fmt.Sprintf("points outside the workspace (%s), so writes through it land outside the mirror this leaf merges", resolved)}
	}
	return LayoutIssue{Path: rel, Kind: LayoutLinkAlias,
		Detail: fmt.Sprintf("aliases %s, which is also in the workspace; the same files are addressable under two different paths", filepath.ToSlash(inside))}
}

// moduleNestingIssue reports the layout where the module path and the
// directory tree disagree by one level, e.g. module "app" with the packages
// under "app/". Import "app/store" then addresses ./store, not ./app/store, so
// the sources are only reachable at "app/app/store".
func moduleNestingIssue(root string, goFiles map[string]int) (LayoutIssue, bool) {
	mod := goModulePath(root)
	if mod == "" {
		return LayoutIssue{}, false
	}
	// Both spellings occur: the full module path used as a directory
	// (github.com/org/app/...) and just its last element (app/...).
	for _, dir := range []string{mod, path.Base(mod)} {
		dir = strings.Trim(path.Clean("/"+dir), "/")
		if dir == "" || dir == "." || strings.Contains(dir, "..") {
			continue
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil || !info.IsDir() {
			continue
		}
		// A nested module is a legitimate layout (monorepo subprojects): its
		// own go.mod owns those import paths.
		if HasGoMod(filepath.Join(root, filepath.FromSlash(dir))) {
			continue
		}
		n := 0
		for d, c := range goFiles {
			if d == dir || strings.HasPrefix(d, dir+"/") {
				n += c
			}
		}
		if n == 0 {
			continue
		}
		return LayoutIssue{Path: dir, Kind: LayoutModuleNesting,
			Detail: fmt.Sprintf("go.mod declares module %q, so import paths %s/<pkg> address ./<pkg>; %d Go file(s) under %q are one level too deep and imports of the natural path cannot resolve", mod, mod, n, dir)}, true
	}
	return LayoutIssue{}, false
}

// goModulePath returns the module path declared by root/go.mod, or "" when
// there is none. Only the leading module directive is needed here, so this
// avoids a full modfile parse (and the dependency) for a diagnostic.
func goModulePath(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return strings.Trim(fields[1], `"'`)
		}
	}
	return ""
}
