package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	// Pure-Go SQLite driver (no cgo toolchain required).
	_ "modernc.org/sqlite"
)

const (
	sqliteDefaultRows = 50
	sqliteMaxRows     = 200
	sqliteMaxCell     = 500
	sqliteTimeout     = 15 * time.Second
)

// SQLiteQuery runs one read-only statement against a workspace SQLite
// database and returns a compact table: a "columns:" header followed by one
// JSON array per row. The connection opens with mode=ro plus query_only, so
// no statement — including PRAGMA writes or ATTACH — can modify the file.
// Only SELECT/WITH/PRAGMA/EXPLAIN are accepted. Large/long values are
// clipped to protect the model context; blobs are reported by size.
func (s *Sandbox) SQLiteQuery(relPath, query string, limit int) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("sqlite_query needs a query")
	}
	if !readOnlySQLiteStatement(query) {
		return "", fmt.Errorf("sqlite_query is read-only: one SELECT/WITH/PRAGMA/EXPLAIN statement only (no ';')")
	}
	if limit <= 0 {
		limit = sqliteDefaultRows
	}
	if limit > sqliteMaxRows {
		limit = sqliteMaxRows
	}

	abs, err := s.Resolve(relPath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory, not a SQLite database", relPath)
	}

	u := &url.URL{Scheme: "file", Path: abs}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(3000)")
	q.Add("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return "", err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), sqliteTimeout)
	defer cancel()

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil || len(cols) == 0 {
		return "", fmt.Errorf("statement returned no result set")
	}
	colTypes, _ := rows.ColumnTypes()

	var data strings.Builder
	enc := json.NewEncoder(&data)
	enc.SetEscapeHTML(false)
	n, truncated := 0, false
	for rows.Next() {
		n++
		if n > limit {
			truncated = true
			break
		}
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		rendered := make([]any, len(cols))
		for i, v := range values {
			rendered[i] = renderSQLiteValue(v, colTypes, i)
		}
		// Encoder appends a newline, matching the "one JSON array per line"
		// format; SetEscapeHTML keeps "<blob>" markers readable.
		if err := enc.Encode(rendered); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "columns: %s\n", strings.Join(cols, " | "))
	if n == 0 {
		b.WriteString("(no rows)\n")
		return strings.TrimRight(b.String(), "\n"), nil
	}
	shown := n
	if truncated {
		shown = limit
	}
	fmt.Fprintf(&b, "rows: %d\n", shown)
	b.WriteString(data.String())
	if truncated {
		fmt.Fprintf(&b, "[more rows beyond the %d-row limit; narrow with WHERE or LIMIT]\n", limit)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// readOnlySQLiteStatement permits a single statement that only reads. A
// trailing semicolon is accepted; any other semicolon (multi-statement
// batching) is rejected.
func readOnlySQLiteStatement(query string) bool {
	q := strings.TrimSpace(query)
	q = strings.TrimSuffix(q, ";")
	q = strings.TrimSpace(q)
	if strings.Contains(q, ";") {
		return false
	}
	upper := strings.ToUpper(q)
	for _, prefix := range []string{"SELECT", "WITH", "PRAGMA", "EXPLAIN"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

// renderSQLiteValue converts a database/sql value into a JSON-friendly
// representation, labeling blobs by size and clipping long text.
func renderSQLiteValue(v any, colTypes []*sql.ColumnType, i int) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		typeName := ""
		if i < len(colTypes) && colTypes[i] != nil {
			typeName = strings.ToUpper(colTypes[i].DatabaseTypeName())
		}
		if typeName == "BLOB" {
			return fmt.Sprintf("<blob %d bytes>", len(t))
		}
		return clipSQLiteCell(string(t))
	case string:
		return clipSQLiteCell(t)
	default:
		return v // int64/float64/bool/time.Time marshal directly
	}
}

func clipSQLiteCell(s string) string {
	if len(s) <= sqliteMaxCell {
		return s
	}
	return s[:sqliteMaxCell] + "...[truncated]"
}
