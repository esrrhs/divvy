package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// RepoMap is a bounded, ranked index of project declarations. It exists
// because a directory listing says what files are called, not which ones
// matter: on a large repository the model otherwise starts with the same
// alphabetical slice of names and spends steps searching. Ranking by how
// often a declaration is referenced elsewhere is language-agnostic and costs
// no embedding index to maintain.
const (
	repoMapMaxFileBytes = 512 << 10
	repoMapMaxFiles     = 3000
	repoMapMaxSymbols   = 160
	repoMapDefaultChars = 4000
)

type repoMapDecl struct {
	rel   string
	line  int
	kind  string
	name  string
	text  string
	refs  int
	score float64
}

// repoMapIdent matches an identifier in non-Go files. Go files use the AST so
// comments and strings do not inflate scores.
var repoMapIdent = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// RepoMap returns a ranked, character-budgeted symbol map, or "" when the
// workspace has no declarations that are referenced by another file. Test
// files, generated files and ignored paths are excluded: they describe how
// the code is checked, not how it is organized.
func (s *Sandbox) RepoMap(maxChars int) string {
	if s == nil || strings.TrimSpace(s.Root) == "" {
		return ""
	}
	if maxChars <= 0 {
		maxChars = repoMapDefaultChars
	}

	type source struct {
		abs string
		rel string
	}
	var sources []source
	_ = filepath.WalkDir(s.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != s.Root && skipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || skipDirNames[d.Name()] || isRepoMapSkipFile(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > repoMapMaxFileBytes {
			return nil
		}
		rel, err := filepath.Rel(s.Root, p)
		if err != nil {
			return nil
		}
		sources = append(sources, source{abs: p, rel: filepath.ToSlash(rel)})
		if len(sources) >= repoMapMaxFiles {
			return filepath.SkipAll
		}
		return nil
	})
	if len(sources) == 0 {
		return ""
	}
	rels := make([]string, 0, len(sources))
	for _, src := range sources {
		rels = append(rels, src.rel)
	}
	ignored := s.IgnoredPaths(rels)

	counts := map[string]int{}
	docFreq := map[string]int{}
	scanned := 0
	var decls []repoMapDecl
	for _, src := range sources {
		if ignored[src.rel] {
			continue
		}
		data, err := os.ReadFile(src.abs)
		if err != nil || IsBinarySniff(data) || !utf8.Valid(data) {
			continue
		}
		if isGeneratedSource(data) {
			continue
		}
		scanned++
		var fileCounts map[string]int
		if strings.HasSuffix(src.rel, ".go") {
			fileDecls, fc := repoMapGoFile(src.abs, src.rel)
			decls = append(decls, fileDecls...)
			fileCounts = fc
		} else {
			text := string(data)
			fileCounts = map[string]int{}
			for _, m := range repoMapIdent.FindAllString(text, -1) {
				fileCounts[m]++
			}
			decls = append(decls, repoMapPatternDecls(src.rel, text)...)
		}
		for name, n := range fileCounts {
			counts[name] += n
			docFreq[name]++
		}
	}

	for i := range decls {
		// One reference is the declaration itself; only definitions other
		// files actually use are worth spending prompt space on.
		decls[i].refs = counts[decls[i].name] - 1
		// Raw counts favor ubiquitous names like Error or Close. Weighting by
		// inverse document frequency keeps project-specific declarations
		// from being buried by language-wide vocabulary.
		if df := docFreq[decls[i].name]; df > 0 && scanned > 0 {
			decls[i].score = float64(decls[i].refs) * math.Log2(1+float64(scanned)/float64(df))
		}
	}
	kept := decls[:0]
	for _, d := range decls {
		if d.refs > 0 && strings.TrimSpace(d.text) != "" {
			kept = append(kept, d)
		}
	}
	decls = kept
	if len(decls) == 0 {
		return ""
	}
	sort.SliceStable(decls, func(i, j int) bool {
		if decls[i].score != decls[j].score {
			return decls[i].score > decls[j].score
		}
		if decls[i].rel != decls[j].rel {
			return decls[i].rel < decls[j].rel
		}
		return decls[i].line < decls[j].line
	})
	if len(decls) > repoMapMaxSymbols {
		decls = decls[:repoMapMaxSymbols]
	}

	const header = "Ranked symbol map — declarations referenced most often by other files (a static index, not a semantic one; use read_file, find_symbol, or search_files for contents):\n"
	var b strings.Builder
	b.WriteString(header)
	used := len(header)
	currentFile := ""
	for _, d := range decls {
		if d.rel != currentFile {
			entry := "\n" + d.rel + "\n"
			if used+len(entry) > maxChars {
				break
			}
			b.WriteString(entry)
			used += len(entry)
			currentFile = d.rel
		}
		entry := "  " + itoa(d.line) + ": " + d.text + "\n"
		if used+len(entry) > maxChars {
			break
		}
		b.WriteString(entry)
		used += len(entry)
	}
	return strings.TrimRight(b.String(), "\n")
}

// repoMapGoFile returns the declarations and identifier counts of one Go
// file. Method names count as identifiers, so a widely used method outranks a
// helper that nothing calls.
func repoMapGoFile(abs, rel string) ([]repoMapDecl, map[string]int) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, abs, nil, 0)
	if err != nil {
		return nil, nil
	}
	var decls []repoMapDecl
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind := "func"
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
			}
			decls = append(decls, repoMapDecl{
				rel:  rel,
				line: fset.Position(d.Pos()).Line,
				kind: kind,
				name: d.Name.Name,
				text: compactGoNode(fset, d),
			})
		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						decls = append(decls, repoMapDecl{
							rel:  rel,
							line: fset.Position(ts.Pos()).Line,
							kind: "type",
							name: ts.Name.Name,
							text: compactGoTypeSpec(fset, ts),
						})
					}
				}
			case token.VAR, token.CONST:
				kind := "var"
				if d.Tok == token.CONST {
					kind = "const"
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, n := range vs.Names {
						decls = append(decls, repoMapDecl{
							rel:  rel,
							line: fset.Position(n.Pos()).Line,
							kind: kind,
							name: n.Name,
							text: n.Name,
						})
					}
				}
			}
		}
	}
	counts := map[string]int{}
	ast.Inspect(file, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			counts[id.Name]++
		}
		return true
	})
	return decls, counts
}

// repoMapPatternDecls reuses the outline declaration patterns for non-Go
// files. Only declarations, not arbitrary identifiers, become map entries.
func repoMapPatternDecls(rel, text string) []repoMapDecl {
	var out []repoMapDecl
	for i, raw := range strings.Split(text, "\n") {
		for _, p := range outlinePatterns {
			m := p.re.FindStringSubmatch(raw)
			if m == nil {
				continue
			}
			out = append(out, repoMapDecl{
				rel:  rel,
				line: i + 1,
				kind: p.kind,
				name: m[1],
				text: clipOutlineText(strings.TrimSpace(raw)),
			})
			break
		}
	}
	return out
}

// isRepoMapSkipFile excludes test and generated companion files. Tests are
// useful context but their helpers are local to the checks, and including
// them would rank assertion helpers above the code under test.
func isRepoMapSkipFile(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, "_test.go") ||
		strings.HasSuffix(lower, ".test.ts") || strings.HasSuffix(lower, ".test.tsx") ||
		strings.HasSuffix(lower, ".spec.ts") || strings.HasSuffix(lower, ".spec.tsx") ||
		strings.HasSuffix(lower, ".test.js") || strings.HasSuffix(lower, ".spec.js") ||
		strings.HasSuffix(lower, ".test.mjs") || strings.HasSuffix(lower, ".spec.mjs") {
		return true
	}
	switch lower {
	case "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "go.sum", "cargo.lock", "poetry.lock":
		return true
	}
	return false
}

func isGeneratedSource(data []byte) bool {
	head := data
	if len(head) > 2048 {
		head = head[:2048]
	}
	text := string(head)
	return strings.Contains(text, "Code generated") && strings.Contains(text, "DO NOT EDIT")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
