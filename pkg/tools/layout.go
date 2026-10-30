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

	// LayoutExtraGoMod: a go.mod was found outside the workspace root. Nested
	// modules are legitimate in monorepos when contracted, but a weak model
	// often drops a second go.mod next to a package by accident; later leaves
	// then burn tokens chasing "wrong module" import errors.
	LayoutExtraGoMod LayoutIssueKind = "extra-go-mod"

	// LayoutContractDrift: declared outputs were expected at one path, but the
	// matching files landed under a module-named nesting (or a similarly wrong
	// prefix). Soft DoD greps against the drifted path then greenwash the leaf
	// and leave every later import broken.
	LayoutContractDrift LayoutIssueKind = "contract-drift"
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

// CheckLeafLayout runs the workspace layout check and then compares declared
// contract outputs against what is actually on disk. It is the fail-fast gate
// used after a leaf verifies (and before isolate merge): soft DoD greps can
// pass against a drifted path, but later leaves cannot fix the imports.
//
// expectedOutputs may be empty; then only CheckLayout issues are returned.
// An error is returned only when root itself cannot be read.
func CheckLeafLayout(root string, expectedOutputs []string) ([]LayoutIssue, error) {
	issues, err := CheckLayout(root)
	if err != nil {
		return nil, err
	}
	issues = append(issues, extraGoModIssues(root)...)
	issues = append(issues, contractDriftIssues(root, expectedOutputs)...)
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Kind != issues[j].Kind {
			return issues[i].Kind < issues[j].Kind
		}
		return issues[i].Path < issues[j].Path
	})
	return issues, nil
}

// FormatLayoutIssues renders issues as a single concrete failure message
// naming found vs expected paths so a leaf retry (or a human) knows what to
// move without rediscovering the drift from compiler noise.
func FormatLayoutIssues(issues []LayoutIssue) string {
	if len(issues) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("layout drift detected — refuse to accept this leaf until paths match the contract:\n")
	for _, is := range issues {
		fmt.Fprintf(&b, "  - [%s] %s: %s\n", is.Kind, is.Path, is.Detail)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ContractNestingOutputs reports declared output paths that would nest under
// the Go module's directory name (e.g. module "physics2d" with output
// "physics2d/physics/x.go"). Used at plan-check time before any leaf runs.
func ContractNestingOutputs(root string, outputs []string) []LayoutIssue {
	mod := goModulePath(root)
	if mod == "" {
		// No go.mod yet: still flag outputs that start with a repeated first
		// segment when the goal named a module that way — but without a
		// module declaration we cannot know the intended name, so skip.
		return nil
	}
	return nestingAgainstModule(mod, outputs)
}

// nestingAgainstModule flags outputs whose first path segment equals the
// module path or its last element, which is exactly the directory shape
// moduleNestingIssue detects after the files exist.
func nestingAgainstModule(mod string, outputs []string) []LayoutIssue {
	if mod == "" {
		return nil
	}
	prefixes := []string{mod, path.Base(mod)}
	seen := map[string]bool{}
	var out []LayoutIssue
	for _, raw := range outputs {
		rel := strings.Trim(strings.ReplaceAll(raw, "\\", "/"), "/")
		if rel == "" || rel == "." {
			continue
		}
		for _, prefix := range prefixes {
			prefix = strings.Trim(path.Clean("/"+prefix), "/")
			if prefix == "" || prefix == "." {
				continue
			}
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				if seen[rel] {
					break
				}
				seen[rel] = true
				out = append(out, LayoutIssue{
					Path: rel,
					Kind: LayoutContractDrift,
					Detail: fmt.Sprintf(
						"declared output nests under module path element %q; with go.mod module %q the file should live at %q (relative to workdir), not under a directory named after the module",
						prefix, mod, strings.TrimPrefix(strings.TrimPrefix(rel, prefix+"/"), prefix),
					),
				})
				break
			}
		}
	}
	return out
}

// extraGoModIssues flags go.mod files outside the workspace root. A nested
// module with its own go.mod is reported so the operator (or a later leaf)
// can confirm it was intentional; CheckLayout already skips nesting checks
// for those directories, but the presence of an unexpected go.mod still
// deserves a leaf-level fail when nothing contracted it.
func extraGoModIssues(root string) []LayoutIssue {
	var issues []LayoutIssue
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if p == root {
				return werr
			}
			return nil
		}
		if d.IsDir() {
			if p != root && skipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" || (d.Type()&fs.ModeSymlink) != 0 {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "go.mod" {
			return nil
		}
		issues = append(issues, LayoutIssue{
			Path: rel,
			Kind: LayoutExtraGoMod,
			Detail: fmt.Sprintf(
				"go.mod should live at the workspace root (or be an explicitly contracted nested module); found %q — move or remove it so imports resolve from the root module",
				rel,
			),
		})
		return nil
	})
	return issues
}

// contractDriftIssues looks for declared outputs that are missing at their
// contracted path but present under a module-named nesting. That is the
// FreeLLM physics failure mode: soft grep DoD passed against
// physics2d/physics/x.go while go.mod said module physics2d, so imports of
// physics2d/physics could never resolve.
func contractDriftIssues(root string, outputs []string) []LayoutIssue {
	if len(outputs) == 0 {
		return nil
	}
	mod := goModulePath(root)
	var prefixes []string
	if mod != "" {
		prefixes = []string{mod, path.Base(mod)}
	}
	var issues []LayoutIssue
	for _, raw := range outputs {
		want := strings.Trim(strings.ReplaceAll(raw, "\\", "/"), "/")
		if want == "" || want == "." {
			continue
		}
		wantPath := filepath.Join(root, filepath.FromSlash(want))
		if fileExists(wantPath) {
			continue
		}
		// Look for the same basename under a module-named prefix.
		base := path.Base(want)
		var found []string
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil || d.IsDir() {
				if werr == nil && d.IsDir() && p != root && skipDirNames[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() != base {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if rel == want {
				return nil
			}
			// Prefer reporting nestings under the module name; also catch any
			// path that ends with the contracted relative path under an extra
			// prefix (e.g. want physics/x.go, found physics2d/physics/x.go).
			if strings.HasSuffix(rel, "/"+want) || rel == want {
				found = append(found, rel)
				return nil
			}
			for _, prefix := range prefixes {
				prefix = strings.Trim(path.Clean("/"+prefix), "/")
				if prefix != "" && (rel == prefix+"/"+want || strings.HasPrefix(rel, prefix+"/")) {
					if path.Base(rel) == base {
						found = append(found, rel)
					}
				}
			}
			return nil
		})
		if len(found) == 0 {
			continue
		}
		sort.Strings(found)
		issues = append(issues, LayoutIssue{
			Path: want,
			Kind: LayoutContractDrift,
			Detail: fmt.Sprintf(
				"expected %q relative to workdir, but found %s instead — move the file(s) to the contracted path (do not nest under the module name)",
				want, strings.Join(found, ", "),
			),
		})
	}
	// Also flag nesting of the declared outputs themselves against the module.
	issues = append(issues, nestingAgainstModule(mod, outputs)...)
	return dedupeLayoutIssues(issues)
}

func fileExists(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.Mode().IsRegular()
}

func dedupeLayoutIssues(in []LayoutIssue) []LayoutIssue {
	seen := map[string]bool{}
	var out []LayoutIssue
	for _, is := range in {
		key := string(is.Kind) + "|" + is.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, is)
	}
	return out
}
