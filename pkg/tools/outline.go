package tools

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxOutlineEntries = 200

// outlineEntry is one top-level declaration in an outline report.
type outlineEntry struct {
	line int
	kind string // func | method | type | var | const
	text string
}

// Outline returns the top-level declaration map of one file: one
// "line: kind text" line per function, method, type, or var/const group.
// Go files are parsed with the real AST and show compact signatures (bodies
// omitted); other languages fall back to declaration patterns. It lets a
// weak model survey an unfamiliar file in one cheap call and then read only
// the relevant line ranges.
func (s *Sandbox) Outline(relPath string) (string, error) {
	abs, err := s.Resolve(relPath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", relPath)
	}

	var entries []outlineEntry
	if strings.HasSuffix(abs, ".go") {
		entries, err = goOutline(abs)
	} else {
		entries, err = patternOutline(abs)
	}
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return fmt.Sprintf("(no top-level declarations in %s)", relPath), nil
	}

	width := len(fmt.Sprint(entries[len(entries)-1].line))
	var b strings.Builder
	shown := 0
	for _, e := range entries {
		if shown >= maxOutlineEntries {
			break
		}
		fmt.Fprintf(&b, "%*d: %s %s\n", width, e.line, e.kind, e.text)
		shown++
	}
	if len(entries) > maxOutlineEntries {
		fmt.Fprintf(&b, "[%d more declaration(s) omitted]\n", len(entries)-maxOutlineEntries)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// goOutline parses one Go file with the AST and renders signature-only
// declarations (function bodies stripped by go/printer).
func goOutline(path string) ([]outlineEntry, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var out []outlineEntry
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind := "func"
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
			}
			out = append(out, outlineEntry{
				line: fset.Position(d.Pos()).Line,
				kind: kind,
				text: compactGoNode(fset, d),
			})
		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						out = append(out, outlineEntry{
							line: fset.Position(ts.Pos()).Line,
							kind: "type",
							text: compactGoTypeSpec(fset, ts),
						})
					}
				}
			case token.VAR, token.CONST:
				kind := "var"
				if d.Tok == token.CONST {
					kind = "const"
				}
				names := genDeclNames(d)
				if names != "" {
					out = append(out, outlineEntry{
						line: fset.Position(d.Pos()).Line,
						kind: kind,
						text: names,
					})
				}
			}
		}
	}
	return out, nil
}

// compactGoNode prints a FuncDecl without doc or body, e.g.
// "func (c *Client) Do(ctx context.Context, req *Request) (*Response, error)".
func compactGoNode(fset *token.FileSet, d *ast.FuncDecl) string {
	clone := *d
	clone.Doc = nil
	clone.Body = nil
	var buf bytes.Buffer
	_ = printer.Fprint(&buf, fset, &clone)
	return clipOutlineText(buf.String())
}

// compactGoTypeSpec renders a type name plus a compact underlying shape
// ("type Widget struct", "type Mode int") rather than dumping a large
// struct/interface body.
func compactGoTypeSpec(fset *token.FileSet, ts *ast.TypeSpec) string {
	switch ts.Type.(type) {
	case *ast.StructType:
		return fmt.Sprintf("type %s struct", ts.Name.Name)
	case *ast.InterfaceType:
		return fmt.Sprintf("type %s interface", ts.Name.Name)
	case *ast.FuncType:
		var buf bytes.Buffer
		buf.WriteString("type ")
		buf.WriteString(ts.Name.Name)
		buf.WriteByte(' ')
		_ = printer.Fprint(&buf, fset, ts.Type)
		return clipOutlineText(buf.String())
	default:
		var buf bytes.Buffer
		buf.WriteString("type ")
		buf.WriteString(ts.Name.Name)
		buf.WriteString(" ")
		_ = printer.Fprint(&buf, fset, ts.Type)
		return clipOutlineText(buf.String())
	}
}

// genDeclNames joins the names declared by a var/const group.
func genDeclNames(d *ast.GenDecl) string {
	var names []string
	for _, spec := range d.Specs {
		if vs, ok := spec.(*ast.ValueSpec); ok {
			for _, n := range vs.Names {
				names = append(names, n.Name)
			}
		}
	}
	return strings.Join(names, ", ")
}

// outlinePatterns matches common declaration lines across non-Go languages.
// Capturing the first group yields the declared name where present.
var outlinePatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	// Python (def/async def/class), JS/TS (function/class/methods), Rust
	// (fn/struct/enum/trait/type), plus generic C-family fallbacks.
	{"func", regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+|public\s+|private\s+|protected\s+|static\s+|export\s+(?:default\s+)?|async\s+)*(?:fn|func|function|def|sub)\s+([A-Za-z_]\w*)`)},
	{"type", regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+|public\s+|private\s+|protected\s+|export\s+(?:default\s+)?|abstract\s+)*(?:class|struct|enum|interface|trait|type|object)\s+([A-Za-z_]\w*)`)},
}

// patternOutline scans a non-Go file line by line for declaration patterns.
// Large files reuse the search scanner's size ceiling.
func patternOutline(path string) ([]outlineEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > maxSearchFileSize {
		return nil, fmt.Errorf("%s exceeds %d-byte outline limit", filepath.Base(path), maxSearchFileSize)
	}
	var out []outlineEntry
	for i, raw := range strings.Split(string(data), "\n") {
		for _, p := range outlinePatterns {
			if p.re.MatchString(raw) {
				out = append(out, outlineEntry{
					line: i + 1,
					kind: p.kind,
					text: clipOutlineText(strings.TrimSpace(raw)),
				})
				break
			}
		}
	}
	return out, nil
}

func clipOutlineText(s string) string {
	const max = 140
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
