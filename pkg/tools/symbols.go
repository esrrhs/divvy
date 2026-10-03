package tools

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// FindSymbol locates definitions of name. Go files use the real AST (kind
// filters func/type/var); other files fall back to a regex search.
// Returns "file:line kind name" lines.
func (s *Sandbox) FindSymbol(ctx context.Context, name, kind, glob string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("find_symbol needs a name")
	}

	var hits []string
	filepath.WalkDir(s.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirNames[d.Name()] && p != s.Root {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(s.Root, p)
		rel = filepath.ToSlash(rel)
		if glob != "" && !simpleGlobMatch(rel, glob) {
			return nil
		}
		if strings.HasSuffix(p, ".go") {
			hits = append(hits, findGoSymbol(p, rel, name, kind)...)
		}
		return nil
	})

	// Non-Go / additional matches: regex fallback across all text files.
	if len(hits) == 0 {
		quoted := regexp.QuoteMeta(name)
		patterns := map[string]string{
			"func": `func\s*(?:\([^)]*\)\s*)?` + quoted + `\s*[(\[]`,
			"type": `(?:type|class|struct|enum|interface)\s+` + quoted + `\b`,
			"var":  `(?:var|const|let)\s+.*\b` + quoted + `\b`,
			"":     `\b` + quoted + `\b`,
		}
		out, err := s.SearchFiles(patterns[strings.ToLower(kind)], "", glob, false)
		if err == nil && !strings.HasPrefix(out, "(no matches") {
			return out, nil
		}
	}

	if len(hits) == 0 {
		return "no match for " + name, nil
	}
	return strings.Join(hits, "\n"), nil
}

// FindReferences performs a zero-dependency, text-level usage scan for name:
// every word-boundary occurrence outside obvious declaration lines (func/
// def/fn, type/class/struct/..., var/const/let definitions). This is NOT
// type-aware: comments and string literals mentioning the name are included
// and aliased/dynamic call sites are missed. That trade-off is deliberate
// (no gopls/toolchain dependency) and disclosed to the model in the tool
// description. Output reuses the search_files "path:line: text" shape.
func (s *Sandbox) FindReferences(name, glob string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("find_symbol needs a name")
	}
	out, err := s.SearchFiles(`\b`+regexp.QuoteMeta(name)+`\b`, "", glob, true)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(out, "(no matches") {
		return out, nil
	}
	decls := declarationLineRegexes(name)
	var kept []string
	for _, line := range rangeLines(out) {
		// SearchFiles' truncation note ("[more than N matches; ...]") is not
		// a match line; pass it through untouched.
		if strings.HasPrefix(line, "[") {
			kept = append(kept, line)
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 {
			text := parts[2]
			isDecl := false
			for _, re := range decls {
				if re.MatchString(text) {
					isDecl = true
					break
				}
			}
			if isDecl {
				continue
			}
		}
		kept = append(kept, line)
	}
	if len(kept) == 0 {
		return fmt.Sprintf("(no references for %q outside declarations)", name), nil
	}
	return strings.Join(kept, "\n"), nil
}

// rangeLines splits text into non-empty lines.
func rangeLines(text string) []string {
	raw := strings.Split(text, "\n")
	out := raw[:0]
	for _, l := range raw {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// declarationLineRegexes builds the per-name patterns that identify
// declaration lines to exclude from reference results.
func declarationLineRegexes(name string) []*regexp.Regexp {
	q := regexp.QuoteMeta(name)
	return []*regexp.Regexp{
		// Go/Python/Rust-style function or method definitions.
		regexp.MustCompile(`\b(?:func|def|fn)\s*(?:\([^)]*\)\s*)?` + q + `\s*[(\[]`),
		// Type/class/struct/enum/interface definitions.
		regexp.MustCompile(`\b(?:type|class|struct|enum|interface)\s+` + q + `\b`),
		// var/const/let definitions (var Foo int / const Foo = ...),
		// including grouped lists (var a, Foo int).
		regexp.MustCompile(`\b(?:var|const|let)\s+[A-Za-z0-9_,\s*]*\b` + q + `\b`),
	}
}

// findGoSymbol parses one Go file and reports matching top-level decls,
// labelling results with the workspace-relative path.
func findGoSymbol(path, label, name, kind string) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil
	}
	kind = strings.ToLower(kind)
	var out []string

	add := func(pos token.Pos, k string) {
		line := fset.Position(pos).Line
		out = append(out, fmt.Sprintf("%s:%d %s %s", label, line, k, name))
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			dn := d.Name.Name
			if dn == name && (kind == "" || kind == "func") {
				add(d.Pos(), "func")
			}
		case *ast.GenDecl:
			k := ""
			switch d.Tok {
			case token.TYPE:
				k = "type"
			case token.VAR, token.CONST:
				k = "var"
			default:
				continue
			}
			if kind != "" && kind != k {
				continue
			}
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					if sp.Name.Name == name {
						add(sp.Pos(), "type")
					}
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						if n.Name == name {
							add(n.Pos(), "var")
						}
					}
				}
			}
		}
	}
	return out
}

// simpleGlobMatch matches the full rel path or just its basename against
// glob, so "*.go" matches nested files as well.
func simpleGlobMatch(rel, glob string) bool {
	glob = filepath.ToSlash(strings.TrimSpace(glob))
	if ok, _ := filepath.Match(glob, rel); ok {
		return true
	}
	ok, _ := filepath.Match(glob, filepath.Base(rel))
	return ok
}
