package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
)

// NFR-5: a routine action must reach an SSE subscriber end-to-end in well
// under 1s on this machine. We measure from issuing POST /plan/approve to
// receiving the first *live* (non-replay, post-subscribe) event frame that
// did not exist when the stream opened.
func TestSSE_LiveEventLatencyUnderSecond(t *testing.T) {
	work, data := t.TempDir(), t.TempDir()
	mgr := agent.NewRunManager()
	// The run must be stopped before the test ends: it keeps working in the
	// background (the approve below releases it), and a run that outlives the
	// test keeps writing into the temp dir — Go's cleanup then fails with
	// "directory not empty" and reports the whole test as failed.
	defer mgr.Shutdown(2 * time.Second)
	h, err := mgr.Start(srvCfg(work, data, "srv_latency"), srvScript(), agent.SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	waitRunPhase(t, h, agent.PhasePlanReview)
	ts := startSSEServer(t, mgr, data)

	req, err := http.NewRequest(http.MethodGet,
		ts.URL+"/api/sessions/srv_latency/events?token=test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sse status=%d", resp.StatusCode)
	}
	r := bufio.NewReader(resp.Body)

	// Drain the replay burst up to and including the authoritative
	// subscribe snapshot; remember the highest seq present at open.
	var baselineSeq int64
	for {
		f := nextFrame(t, r, time.Second)
		if f.data == "" {
			continue // heartbeat
		}
		var head struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
			Seq    int64  `json:"seq"`
		}
		if err := json.Unmarshal([]byte(f.data), &head); err != nil {
			t.Fatal(err)
		}
		if head.Seq > baselineSeq {
			baselineSeq = head.Seq
		}
		if head.Kind == "tree_snapshot" && head.Reason == "subscribe" {
			break
		}
	}

	t0 := time.Now()
	status, _ := apiDo(t, http.MethodPost,
		apiBase(ts)+"/sessions/srv_latency/plan/approve?token=test-token", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("approve: %d", status)
	}

	// First live frame newer than the open baseline is the observed
	// end-to-end delivery.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f := nextFrame(t, r, time.Second)
		if f.data == "" {
			continue
		}
		var head struct {
			Seq    int64 `json:"seq"`
			Replay bool  `json:"replay"`
		}
		if err := json.Unmarshal([]byte(f.data), &head); err != nil {
			t.Fatal(err)
		}
		if head.Replay || head.Seq <= baselineSeq {
			continue
		}
		elapsed := time.Since(t0)
		if elapsed >= time.Second {
			t.Fatalf("live event delivered in %s, NFR-5 requires <1s", elapsed)
		}
		t.Logf("approve → first live SSE event (seq %d): %s", head.Seq, elapsed)
		return
	}
	t.Fatal("no post-approve live event within 2s")
}
