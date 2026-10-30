package tools

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// configureCacheBasenames are generated files that embed absolute configure-time
// paths. Merging them from an isolate mirror into the shared workdir (or
// snapshotting them the other way) poisons the next cmake/meson/ninja run with
// "cache directory mismatch" / wrong source-tree errors. They are cheap to
// regenerate, so ScrubConfigureCaches deletes them instead of rewriting paths.
var configureCacheBasenames = map[string]bool{
	"CMakeCache.txt":        true,
	"cmake.check_cache":     true,
	"CTestTestfile.cmake":   true,
	"cmake_install.cmake":   true,
	"build.ninja":           true,
	"rules.ninja":           true,
	".ninja_deps":           true,
	".ninja_log":            true,
	"compile_commands.json": true,
}

// configureCacheDirNames are generated directories whose contents also embed
// absolute paths (CMakeFiles/CMakeConfigureLog.yaml, meson-private/*, …).
var configureCacheDirNames = map[string]bool{
	"CMakeFiles":      true,
	"meson-private":   true,
	"meson-info":      true,
	"meson-logs":      true,
	".mesonpy":        true,
	".mesonpy-native": true,
}

// isConfigureCacheRel reports whether a slash-separated workspace-relative path
// is a configure/build-system cache artifact that must not cross the
// mirror↔workdir boundary.
func isConfigureCacheRel(rel string) bool {
	rel = filepath.ToSlash(rel)
	if rel == "" {
		return false
	}
	base := rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		base = rel[i+1:]
	}
	if configureCacheBasenames[base] {
		return true
	}
	for _, seg := range strings.Split(rel, "/") {
		if configureCacheDirNames[seg] {
			return true
		}
	}
	return false
}

// ScrubConfigureCaches removes CMake/Meson/Ninja configure outputs under root
// that embed absolute paths. It skips the same ignored directories as mirrors
// (.git, node_modules, …). Returns the workspace-relative paths that were
// removed (files and directories). Missing paths are fine; other errors abort.
func ScrubConfigureCaches(root string) (removed []string, err error) {
	if root == "" {
		return nil, nil
	}
	var dirHits []string
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == root {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if skipDirNames[name] {
				return filepath.SkipDir
			}
			if configureCacheDirNames[name] {
				rel, rerr := filepath.Rel(root, p)
				if rerr != nil {
					return rerr
				}
				dirHits = append(dirHits, filepath.ToSlash(rel))
				return filepath.SkipDir
			}
			return nil
		}
		if skipDirNames[name] || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if !isConfigureCacheRel(rel) {
			return nil
		}
		if rmErr := os.Remove(p); rmErr != nil && !os.IsNotExist(rmErr) {
			return rmErr
		}
		removed = append(removed, rel)
		return nil
	})
	if walkErr != nil {
		return removed, walkErr
	}
	// Deepest directories first so nested hits cannot leave a parent half-deleted.
	sort.Slice(dirHits, func(i, j int) bool {
		return strings.Count(dirHits[i], "/") > strings.Count(dirHits[j], "/")
	})
	for _, rel := range dirHits {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if rmErr := os.RemoveAll(p); rmErr != nil && !os.IsNotExist(rmErr) {
			return removed, rmErr
		}
		removed = append(removed, rel)
	}
	return removed, nil
}
