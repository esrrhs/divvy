package tools

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// newWritableSQLite opens a fresh temp database (write access, used only to
// seed fixture data for the read-only tool) and returns its workspace path.
func newWritableSQLite(t *testing.T, sb *Sandbox, rel string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(sb.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, avatar BLOB, nickname TEXT)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSQLiteQuery_Tool(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	db := newWritableSQLite(t, sb, "data.db")
	longName := strings.Repeat("z", sqliteMaxCell+50)
	rows := [][]any{
		{1, "alice", []byte{0x89, 0x50, 0x4e, 0x47}, nil},
		{2, "bob", nil, "bobby"},
		{3, longName, nil, nil},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO users(id,name,avatar,nickname) VALUES(?,?,?,?)`, r...); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	out, err := sb.Call(ctx, ToolSQLiteQuery, map[string]any{
		"path":  "data.db",
		"query": "SELECT id, name, avatar, nickname FROM users ORDER BY id",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"columns: id | name | avatar | nickname",
		"rows: 3",
		`[1,"alice","<blob 4 bytes>",null]`,
		`[2,"bob",null,"bobby"]`,
		"...[truncated]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSQLiteQuery_LimitAndNoRows(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	db := newWritableSQLite(t, sb, "data.db")
	for i := 0; i < 5; i++ {
		if _, err := db.Exec(`INSERT INTO users(name) VALUES(?)`, "n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	out, err := sb.Call(ctx, ToolSQLiteQuery, map[string]any{
		"path": "data.db", "query": "SELECT id FROM users", "limit": 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\n[1]\n") || !strings.Contains(out, "\n[2]\n") ||
		strings.Contains(out, "[3]") || !strings.Contains(out, "rows: 2") ||
		!strings.Contains(out, "more rows beyond the 2-row limit") {
		t.Fatalf("expected exactly 2 shown rows plus truncation note:\n%s", out)
	}

	out, err = sb.Call(ctx, ToolSQLiteQuery, map[string]any{
		"path": "data.db", "query": "SELECT id FROM users WHERE id > 1000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(no rows)") {
		t.Fatalf("expected no-rows marker:\n%s", out)
	}

	// PRAGMA returns schema rows and is permitted.
	out, err = sb.Call(ctx, ToolSQLiteQuery, map[string]any{
		"path": "data.db", "query": "PRAGMA table_info(users)",
	})
	if err != nil || !strings.Contains(out, "name") {
		t.Fatalf("pragma should work: %v\n%s", err, out)
	}
}

func TestSQLiteQuery_ReadOnlyAndValidation(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	db := newWritableSQLite(t, sb, "data.db")
	if _, err := db.Exec(`INSERT INTO users(name) VALUES('keep')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Writes and DDL are refused before opening the connection.
	for _, q := range []string{
		"DELETE FROM users",
		"UPDATE users SET name='x'",
		"DROP TABLE users",
		"INSERT INTO users(name) VALUES('x')",
		"SELECT 1; DROP TABLE users",
	} {
		if _, err := sb.Call(ctx, ToolSQLiteQuery, map[string]any{"path": "data.db", "query": q}); err == nil ||
			!strings.Contains(err.Error(), "read-only") {
			t.Fatalf("query %q must be refused as non-read-only, got: %v", q, err)
		}
	}
	// A trailing semicolon on a read query is tolerated.
	if _, err := sb.Call(ctx, ToolSQLiteQuery, map[string]any{
		"path": "data.db", "query": "SELECT COUNT(*) FROM users;",
	}); err != nil {
		t.Fatalf("trailing semicolon should be allowed: %v", err)
	}

	// The fixture row must still be there.
	out, err := sb.Call(ctx, ToolSQLiteQuery, map[string]any{
		"path": "data.db", "query": "SELECT name FROM users",
	})
	if err != nil || !strings.Contains(out, "keep") {
		t.Fatalf("database must be unchanged:\n%v\n%s", err, out)
	}

	// Bad paths and inputs.
	for _, args := range []map[string]any{
		{"path": "missing.db", "query": "SELECT 1"},
		{"path": "../escape.db", "query": "SELECT 1"},
		{"path": "data.db", "query": ""},
	} {
		if _, err := sb.Call(ctx, ToolSQLiteQuery, args); err == nil {
			t.Fatalf("expected error for %v", args)
		}
	}
	// Pointing at a directory fails clearly.
	if _, err := sb.Call(ctx, ToolSQLiteQuery, map[string]any{"path": ".", "query": "SELECT 1"}); err == nil {
		t.Fatal("directory path must fail")
	}
}
