package server

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Subprocess crash window: the child writes the first rollback target and
// is then killed before Applied is persisted. This is not a power-loss or
// fsync durability test. The process killed is only the helper we started.
func TestRollbackJournal_SubprocessCrashAfterWriteBeforeMark(t *testing.T) {
	if os.Getenv("DIVVY_RB_CRASH_HELPER") == "1" {
		runUnrecordedApplyCrashHelper()
		os.Exit(6)
	}

	work, data := t.TempDir(), t.TempDir()
	id, sid := seedRollbackFixture(t, work, data, []seedFile{
		{path: "a.txt", status: "modified", old: "A-old\n", new: "A-new\n"},
		{path: "b.txt", status: "modified", old: "B-old\n", new: "B-new\n"},
	}, map[string]string{"keep.txt": "keep\n"})
	ready := filepath.Join(data, "crash-ready")

	cmd := exec.Command(os.Args[0], "-test.run=^TestRollbackJournal_SubprocessCrashAfterWriteBeforeMark$", "-test.v=false", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"DIVVY_RB_CRASH_HELPER=1",
		"DIVVY_RB_WORK="+work,
		"DIVVY_RB_DATA="+data,
		"DIVVY_RB_CONV="+id,
		"DIVVY_RB_SID="+sid,
		"DIVVY_RB_READY="+ready,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		waited = true
		t.Fatalf("helper did not finish the unrecorded write: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = cmd.Wait()
	waited = true

	flags := journalAppliedFlags(t, data, id)
	if len(flags) != 2 || flags[0] || flags[1] {
		t.Fatalf("killed helper must leave Applied unset, got %v", flags)
	}
	if mustRead(t, work, "a.txt") != "A-old\n" {
		t.Fatalf("helper should have written the first target, a=%q", mustRead(t, work, "a.txt"))
	}

	ts := apiServer(t, work, data)
	_, got := apiDo(t, http.MethodGet, apiBase(ts)+"/conversations/"+id+"?token=test-token", nil)
	if mustRead(t, work, "a.txt") != "A-new\n" {
		t.Fatalf("restart must compensate the unrecorded write, a=%q", mustRead(t, work, "a.txt"))
	}
	if mustRead(t, work, "b.txt") != "B-new\n" || mustRead(t, work, "keep.txt") != "keep\n" {
		t.Fatalf("unapplied/unrelated files changed")
	}
	c, err := loadConversation(data, id)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(c.RolledBackTurns, sid) {
		t.Fatalf("must not mark rolled_back_turns after compensating a crash window: %v", c.RolledBackTurns)
	}
	if journalExists(data, id) {
		t.Fatal("journal should be cleared after successful compensation")
	}
	if got["pending_rollback"] != nil {
		t.Fatalf("pending after compensate: %v", got["pending_rollback"])
	}
}

func runUnrecordedApplyCrashHelper() {
	work := os.Getenv("DIVVY_RB_WORK")
	data := os.Getenv("DIVVY_RB_DATA")
	conv := os.Getenv("DIVVY_RB_CONV")
	sid := os.Getenv("DIVVY_RB_SID")
	ready := os.Getenv("DIVVY_RB_READY")
	j, err := prepareRollbackJournal(data, conv, sid, work, []rollbackOp{
		{path: "a.txt", restore: "A-old\n"},
		{path: "b.txt", restore: "B-old\n"},
	})
	if err != nil {
		os.Exit(3)
	}
	if err := atomicWriteWorkdirFile(work, "a.txt", "A-old\n"); err != nil {
		os.Exit(4)
	}
	// Leave Applied unset and do not save again — then wait to be killed.
	_ = j
	if err := os.WriteFile(ready, []byte("written"), 0644); err != nil {
		os.Exit(5)
	}
	time.Sleep(60 * time.Second)
}
