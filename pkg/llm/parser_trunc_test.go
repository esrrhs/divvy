package llm

import (
	"strings"
	"testing"
)

func TestExtractJSON_TruncatedWriteFileContent(t *testing.T) {
	raw := `{"thought":"write","action":"write_file","args":{"path":"a.go","content":"package main\n\nfunc main() {\n`
	got, err := ExtractJSON(raw)
	if err != nil {
		t.Fatalf("expected truncated write_file JSON to close: %v", err)
	}
	act, err := ParseAction(got)
	if err != nil {
		t.Fatalf("ParseAction after repair: %v (raw=%s)", err, got)
	}
	if act.Name != "write_file" {
		t.Fatalf("name=%s", act.Name)
	}
	content, _ := act.Args["content"].(string)
	if !strings.Contains(content, "package main") {
		t.Fatalf("content not recovered: %#v args=%v", content, act.Args)
	}
	path, _ := act.Args["path"].(string)
	if path != "a.go" {
		t.Fatalf("path=%q", path)
	}
}

func TestParseAction_MissingNameStillErrors(t *testing.T) {
	if _, err := ParseAction(`{"thought":"x","args":{"path":"a.go"}}`); err == nil {
		t.Fatal("missing action name must still error")
	}
}
