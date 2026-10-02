package tools

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// JSONQuery reads path (a workspace JSON file) and extracts the value at
// query, a dotted/bracket path ("items", "items.0.name", "a[0].b"). The
// compact result avoids reading a large JSON into the model context.
func (s *Sandbox) JSONQuery(path, query string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("json_query needs a path")
	}
	raw, err := s.ReadFile(path)
	if err != nil {
		return "", err
	}
	var data any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}

	value, found := walkJSONPath(data, query)
	if !found {
		return "", fmt.Errorf("path %q not found in %s", query, path)
	}

	switch v := value.(type) {
	case string:
		return v, nil
	default:
		out, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v), nil
		}
		return string(out), nil
	}
}

// walkJSONPath navigates data per query. Keys may contain dots when written
// with brackets ["my.key"]. Empty query returns the whole document.
func walkJSONPath(data any, query string) (any, bool) {
	cur := data
	for _, tok := range splitJSONPath(query) {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[tok]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// splitJSONPath splits "a.b[0].c" / `a["x.y"]` into ["a","b","0","c"].
func splitJSONPath(query string) []string {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	var toks []string
	var key strings.Builder
	flush := func() {
		if key.Len() > 0 {
			toks = append(toks, key.String())
			key.Reset()
		}
	}
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch c {
		case '.':
			flush()
		case '[':
			flush()
			// Bracket: numeric index or quoted key.
			if i+1 < len(query) && (query[i+1] == '"' || query[i+1] == '\'') {
				quote := query[i+1]
				end := strings.IndexByte(query[i+2:], quote)
				if end < 0 {
					key.WriteString(query[i:])
					i = len(query)
					continue
				}
				key.WriteString(query[i+2 : i+2+end])
				// i lands on ']'; increment reaches the char after it.
				i = i + 2 + end + 1
			} else {
				end := strings.IndexByte(query[i+1:], ']')
				if end < 0 {
					key.WriteString(query[i+1:])
					i = len(query)
					continue
				}
				key.WriteString(query[i+1 : i+1+end])
				// Point at ']'; the loop increment lands on the next char.
				i = i + end + 1
			}
		default:
			key.WriteByte(c)
		}
	}
	flush()
	return toks
}
