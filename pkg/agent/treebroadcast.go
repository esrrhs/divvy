package agent

import (
	"encoding/json"
	"sync"
	"time"
)

// treeSnapshotCoalesce merges rapid state transitions (a leaf can move
// RUNNING -> VERIFYING within milliseconds) into one tree_snapshot event, so
// the SSE fan-out is not flooded with full-tree payloads.
const treeSnapshotCoalesce = 100 * time.Millisecond

// treeBroadcaster publishes the live task tree to event subscribers. State
// transitions are coalesced (a debounced goroutine emits one snapshot per
// burst); checkpoints publish synchronously so a persisted tree and its
// snapshot event never disagree.
type treeBroadcaster struct {
	o *Orchestrator

	trigger chan string
	stop    chan struct{}
	done    chan struct{}

	mu      sync.Mutex
	started bool
}

func newTreeBroadcaster(o *Orchestrator) *treeBroadcaster {
	return &treeBroadcaster{
		o:       o,
		trigger: make(chan string, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// start launches the coalescing goroutine. Safe to call once per
// orchestrator; later calls are no-ops.
func (b *treeBroadcaster) start() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return
	}
	b.started = true
	go b.loop()
}

// mark requests a coalesced snapshot with the given reason. It never blocks
// the scheduler callback: a pending request absorbs further marks.
func (b *treeBroadcaster) mark(reason string) {
	if b == nil {
		return
	}
	select {
	case b.trigger <- reason:
	default:
	}
}

// loop waits for a dirty signal, then keeps resetting a short timer while
// fresh signals arrive, emitting one snapshot per burst.
func (b *treeBroadcaster) loop() {
	defer close(b.done)
	for {
		select {
		case <-b.stop:
			return
		case reason := <-b.trigger:
			timer := time.NewTimer(treeSnapshotCoalesce)
		drain:
			for {
				select {
				case <-b.stop:
					timer.Stop()
					return
				case r := <-b.trigger:
					reason = r
					if !timer.Stop() {
						<-timer.C // drain the fired value before re-arming
					}
					timer.Reset(treeSnapshotCoalesce)
				case <-timer.C:
					break drain
				}
			}
			b.o.publishTreeSnapshot(reason)
		}
	}
}

// close stops the coalescer. In-flight coalesced snapshots are dropped; the
// final checkpoint/session_end events already carry the terminal state.
// Safe to call multiple times.
func (b *treeBroadcaster) close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if !b.started {
		b.mu.Unlock()
		return
	}
	b.started = false
	b.mu.Unlock()
	close(b.stop)
	<-b.done
}

// publishTreeSnapshot serializes the full tree into one event. The tree is
// embedded as raw JSON so subscribers get one self-contained object rather
// than a string they must parse again.
func (o *Orchestrator) publishTreeSnapshot(reason string) {
	if o == nil || o.events == nil || o.tree == nil {
		return
	}
	data, err := o.tree.ToJSON()
	if err != nil {
		return
	}
	o.events.Record("tree_snapshot", "", map[string]any{
		"reason": reason,
		"tree":   json.RawMessage(data),
	})
}
