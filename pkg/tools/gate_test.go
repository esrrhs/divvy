package tools

import (
	"context"
	"strings"
	"testing"
)

func TestPreFinishGate_NonRepoAndClean(t *testing.T) {
	// Outside a repository the gate is simply absent.
	plain, _ := NewSandbox(t.TempDir())
	gate, err := plain.PreFinishGate(context.Background())
	if err != nil || gate != nil {
		t.Fatalf("non-repo: expected nil gate, got %v, %v", gate, err)
	}

	// A repo with no working-tree changes is clean.
	sb := setupGitRepo(t, map[string]string{"main.go": "package main\n"})
	gate, err = sb.PreFinishGate(context.Background())
	if err != nil || gate != nil {
		t.Fatalf("clean repo: expected nil gate, got %v, %v", gate, err)
	}
}

func TestPreFinishGate_TrackedBlocking(t *testing.T) {
	sb := setupGitRepo(t, map[string]string{"main.go": "package main\n"})
	// Tracked file modified with an unresolved conflict marker.
	if err := sb.WriteFile("main.go", "package main\n\nfunc main() {\n<<<<<<< HEAD\nx()\n=======\ny()\n>>>>>>> b\n}\n"); err != nil {
		t.Fatal(err)
	}
	gate, err := sb.PreFinishGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gate == nil || !gate.Blocking {
		t.Fatalf("conflict marker must block finish: %+v", gate)
	}
	if !strings.Contains(gate.Report, "[conflict] main.go:") {
		t.Fatalf("report missing conflict location:\n%s", gate.Report)
	}
}

func TestPreFinishGate_UntrackedSecretBlocked(t *testing.T) {
	sb := setupGitRepo(t, map[string]string{"main.go": "package main\n"})
	// A brand-new file never appears in `git diff`; the gate must still see
	// the hard-coded secret inside it.
	if err := sb.WriteFile("config/creds.go", "package config\n\nvar token = \"abcdef1234567890\"\n"); err != nil {
		t.Fatal(err)
	}
	gate, err := sb.PreFinishGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gate == nil || !gate.Blocking {
		t.Fatalf("secret in untracked file must block finish: %+v", gate)
	}
	if !strings.Contains(gate.Report, "[secret] config/creds.go:") {
		t.Fatalf("report missing untracked-file secret:\n%s", gate.Report)
	}
}

func TestPreFinishGate_DebugOnlyWarns(t *testing.T) {
	sb := setupGitRepo(t, map[string]string{"main.go": "package main\n"})
	if err := sb.WriteFile("main.go", "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"x\") }\n"); err != nil {
		t.Fatal(err)
	}
	gate, err := sb.PreFinishGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gate == nil {
		t.Fatal("debug leftover should still produce a (non-blocking) gate")
	}
	if gate.Blocking {
		t.Fatalf("debug print must not block finish:\n%s", gate.Report)
	}
	if !strings.Contains(gate.Report, "[debug]") {
		t.Fatalf("debug finding missing:\n%s", gate.Report)
	}
}

func TestPreFinishGate_BinaryUntrackedSkipped(t *testing.T) {
	sb := setupGitRepo(t, map[string]string{"main.go": "package main\n"})
	// Non-UTF8 untracked content must not crash or falsely block the gate.
	if err := writeBinary(sb.Root+"/blob.dat", []byte{0x00, 0x01, 0xff, 0xfe}); err != nil {
		t.Fatal(err)
	}
	gate, err := sb.PreFinishGate(context.Background())
	if err != nil {
		t.Fatalf("binary untracked file must be skipped, got %v", err)
	}
	if gate != nil {
		t.Fatalf("binary-only untracked file should yield clean gate, got %+v", gate)
	}
}
