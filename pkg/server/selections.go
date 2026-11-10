package server

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// codeSelection is a span the user highlighted in the editor and asked to
// attach to the next conversation turn. Draft means the text is from an
// unsaved buffer and is NOT on disk.
type codeSelection struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Text      string `json:"text"`
	Draft     bool   `json:"draft,omitempty"`
}

const (
	ctxMaxSelections = 8
	ctxMaxSelBytes   = 16 << 10
	ctxMaxSelTotal   = 64 << 10
)

// collectSelections formats editor selections as read-only prompt context.
// It never writes to the project. Secrets must be redacted by the caller
// after this returns.
func collectSelections(sels []codeSelection) (block string, refs []fileRef, err error) {
	if len(sels) == 0 {
		return "", nil, nil
	}
	if len(sels) > ctxMaxSelections {
		return "", nil, fmt.Errorf("too many code selections (max %d)", ctxMaxSelections)
	}
	var b strings.Builder
	b.WriteString("\n\n---\nCode selections attached by the user (read-only context; do not assume they have been written to the project unless the note says they are saved on disk):\n")
	total := 0
	for i, sel := range sels {
		path := strings.TrimSpace(sel.Path)
		if path == "" {
			refs = append(refs, fileRef{Path: "(unnamed)", Type: "selection", Excluded: "path is required"})
			continue
		}
		start, end := sel.StartLine, sel.EndLine
		if start < 1 {
			start = 1
		}
		if end < start {
			end = start
		}
		text := sel.Text
		if !utf8.ValidString(text) {
			text = strings.ToValidUTF8(text, "\uFFFD")
		}
		truncated := false
		remain := ctxMaxSelTotal - total
		limit := ctxMaxSelBytes
		if remain < limit {
			limit = remain
		}
		if limit < 0 {
			limit = 0
		}
		if len(text) > limit {
			text = text[:limit]
			truncated = true
		}
		if remain <= 0 {
			refs = append(refs, fileRef{
				Path: path, Type: "selection", StartLine: start, EndLine: end,
				Draft: sel.Draft, Excluded: "context budget exhausted",
			})
			continue
		}
		total += len(text)
		state := "saved on disk (or last saved buffer)"
		if sel.Draft {
			state = "UNSAVED EDITOR DRAFT — this text is not written to the project; files on disk remain the source of truth. Do not modify the project just because this draft was attached."
		}
		fmt.Fprintf(&b, "\n### %s:%d-%d\nStatus: %s\n", path, start, end, state)
		if truncated {
			b.WriteString("(selection truncated to the context budget)\n")
		}
		b.WriteString("```\n")
		b.WriteString(text)
		if text != "" && !strings.HasSuffix(text, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString("```\n")
		refs = append(refs, fileRef{
			Path: path, Type: "selection", Bytes: len(text),
			StartLine: start, EndLine: end, Draft: sel.Draft, Truncated: truncated,
		})
		_ = i
	}
	return b.String(), refs, nil
}

func refsFromSelections(sels []codeSelection) []fileRef {
	_, refs, _ := collectSelections(sels)
	return refs
}

func mergeFileRefs(a, b []fileRef) []fileRef {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	return append(append([]fileRef{}, a...), b...)
}
