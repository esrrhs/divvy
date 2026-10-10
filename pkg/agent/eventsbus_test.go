package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/engine"
)

func TestEventRecorder_SubscribeReceivesFanOut(t *testing.T) {
	rec, err := NewEventRecorder(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	ch1, cancel1 := rec.Subscribe()
	ch2, cancel2 := rec.Subscribe()
	defer cancel1()
	defer cancel2()
	if got := rec.SubscriberCount(); got != 2 {
		t.Fatalf("subscribers = %d, want 2", got)
	}

	rec.Record("tool_call", "leaf-1", map[string]any{"tool": "read_file"})

	for name, ch := range map[string]<-chan map[string]any{"s1": ch1, "s2": ch2} {
		select {
		case ev := <-ch:
			if ev["kind"] != "tool_call" || ev["node_id"] != "leaf-1" || ev["tool"] != "read_file" {
				t.Fatalf("%s got %v", name, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s did not receive event", name)
		}
	}

	cancel1()
	select {
	case _, ok := <-ch1:
		if ok {
			t.Fatal("cancelled channel should be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after cancel")
	}
	if got := rec.SubscriberCount(); got != 1 {
		t.Fatalf("subscribers after cancel = %d, want 1", got)
	}
}

func TestEventRecorder_SlowSubscriberDropsAndNotifies(t *testing.T) {
	rec, err := NewEventRecorder(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	ch, cancel := rec.Subscribe()
	defer cancel()

	// Fill the buffer, then keep going: recording must never block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < eventBuffer+50; i++ {
			rec.Record("tool_call", "n", map[string]any{"i": i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked on a full subscriber buffer")
	}

	// Free one slot, then record one more event: that fan-out first queues
	// the _dropped notice (it reports what was missed before resuming).
	<-ch
	rec.Record("tool_call", "n", map[string]any{"i": 999})

	sawDropped := false
sawNotice:
	for {
		select {
		case ev := <-ch:
			if ev["kind"] == "_dropped" {
				if n, _ := ev["count"].(int); n <= 0 {
					t.Fatalf("_dropped count = %v, want > 0", ev["count"])
				}
				sawDropped = true
				break sawNotice
			}
		case <-time.After(time.Second):
			break sawNotice
		}
	}
	if !sawDropped {
		t.Fatal("slow subscriber never received a _dropped notice")
	}
}

func TestEventRecorder_CloseUnsubscribes(t *testing.T) {
	rec, err := NewEventRecorder(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := rec.Subscribe()
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-ch; ok {
		t.Fatal("subscriber channel must close on recorder Close")
	}
	if rec.SubscriberCount() != 0 {
		t.Fatal("Close must detach subscribers")
	}
}

func TestEventRecorder_ScrubsSecretsBeforePersist(t *testing.T) {
	secret := "sk-leaf-secret-1234abcd"
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, err := NewEventRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	rec.AddSecret(secret)
	ch, cancel := rec.Subscribe()
	defer cancel()
	// Secret echoes through a field that surfaces tool output, nested in a
	// slice, and in a RawMessage payload: every copy must be masked both on
	// disk and in the fan-out.
	rec.Record("tool_call", "root", map[string]any{
		"output_preview": "result: " + secret,
		"nested":         []any{map[string]any{"q": secret}},
	})
	select {
	case ev := <-ch:
		raw, _ := json.Marshal(ev)
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("secret survived fan-out: %s", raw)
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	onDisk, _ := os.ReadFile(path)
	if bytes.Contains(onDisk, []byte(secret)) {
		t.Fatalf("secret survived on disk: %s", onDisk)
	}
	if !bytes.Contains(onDisk, []byte(secretMask)) {
		t.Fatal("redaction marker missing on disk")
	}
}

func TestEventRecorder_SeqContinuesAcrossOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, err := NewEventRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		rec.Record("test", "", map[string]any{"i": i})
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	// A resumed run reopens the same file: numbering continues at 6 rather
	// than restarting, which is what SSE dedup/Last-Event-ID correctness
	// hinges on.
	rec2, err := NewEventRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rec2.Close()
	ch, cancel := rec2.Subscribe()
	defer cancel()
	rec2.Record("test", "", map[string]any{"i": 6})
	select {
	case ev := <-ch:
		if ev["seq"] != int64(6) {
			t.Fatalf("want seq 6 after reopen, got %v", ev["seq"])
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
}

func TestTreeBroadcaster_PublishesSnapshot(t *testing.T) {
	rec, err := NewEventRecorder(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	ch, cancel := rec.Subscribe()
	defer cancel()

	tree := engine.NewTaskTree("sess_x", "goal", "goal")
	o := &Orchestrator{events: rec, tree: tree}
	o.publishTreeSnapshot("checkpoint")

	select {
	case ev := <-ch:
		if ev["kind"] != "tree_snapshot" || ev["reason"] != "checkpoint" {
			t.Fatalf("unexpected event: %v", ev)
		}
		if ev["tree"] == nil {
			t.Fatal("snapshot missing tree payload")
		}
		// The payload must re-marshal inline as a JSON object (RawMessage /
		// jsontext.Value), not as an escaped string.
		raw, merr := json.Marshal(ev)
		if merr != nil || !strings.Contains(string(raw), `"tree":{"id":"sess_x"`) {
			t.Fatalf("tree not embedded as JSON object: %s", raw)
		}
	case <-time.After(time.Second):
		t.Fatal("no tree_snapshot received")
	}
}

func TestTreeBroadcaster_CoalescesRapidMarks(t *testing.T) {
	rec, err := NewEventRecorder(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	tree := engine.NewTaskTree("sess_y", "goal", "goal")
	o := &Orchestrator{events: rec, tree: tree}
	bc := newTreeBroadcaster(o)
	bc.start()
	defer bc.close()

	ch, cancel := rec.Subscribe()
	defer cancel()

	// Fire the burst from one tightly scheduled loop; marks made before the
	// coalesce window elapses must collapse into one snapshot. (The exact
	// surviving reason is not guaranteed across goroutine scheduling, but
	// "one snapshot per burst" is the guarantee under test.)
	burstDone := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			bc.mark("state")
		}
		close(burstDone)
	}()
	<-burstDone

waitFirst:
	for {
		select {
		case ev := <-ch:
			if ev["kind"] == "tree_snapshot" {
				break waitFirst
			}
		case <-time.After(time.Second):
			t.Fatal("no coalesced snapshot emitted")
		}
	}

	// After the burst settles there must be silence (no queued follow-ups).
	select {
	case ev := <-ch:
		if ev["kind"] == "tree_snapshot" {
			t.Fatalf("burst produced more than one snapshot: %v", ev)
		}
	case <-time.After(3 * treeSnapshotCoalesce):
	}
}

func TestTreeBroadcaster_ReasonPassThrough(t *testing.T) {
	rec, err := NewEventRecorder(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	tree := engine.NewTaskTree("sess_z", "goal", "goal")
	o := &Orchestrator{events: rec, tree: tree}
	bc := newTreeBroadcaster(o)
	bc.start()
	defer bc.close()

	ch, cancel := rec.Subscribe()
	defer cancel()

	bc.mark("checkpoint-flagged")
	select {
	case ev := <-ch:
		if ev["kind"] != "tree_snapshot" || ev["reason"] != "checkpoint-flagged" {
			t.Fatalf("got %v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no snapshot emitted")
	}
}

func TestLogger_SubscribeLines(t *testing.T) {
	l := SilentLogger()
	ch, cancel := l.SubscribeLines()
	defer cancel()

	l.Infof("hello %s", "world")
	l.Warnf("careful")
	want := []string{"hello world", "careful"}
	for _, w := range want {
		select {
		case line := <-ch:
			if !strings.Contains(line, w) {
				t.Fatalf("line %q does not contain %q", line, w)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing log line containing %q", w)
		}
	}
}
