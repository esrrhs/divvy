package agent

import (
	"testing"
	"time"

	"github.com/esrrhs/divvy/pkg/models"
)

func TestIsHardLeaf(t *testing.T) {
	easy := &models.TaskNode{ID: "store", Title: "KV store", Description: "in-memory map"}
	if isHardLeaf(easy) {
		t.Fatal("plain store leaf must not be hard")
	}
	hard := &models.TaskNode{
		ID: "integrate", Title: "Wire server",
		DoD: models.DoD{Commands: []string{
			`go build -o /tmp/app . && (/tmp/app & SRV=$!; sleep 1; curl -sf http://127.0.0.1:8080/health; kill $SRV)`,
		}},
	}
	if !isHardLeaf(hard) {
		t.Fatal("integrate+curl smoke must be hard")
	}
	cmake := &models.TaskNode{
		ID: "server", Title: "C++ server",
		DoD: models.DoD{Commands: []string{"ctest --test-dir build --output-on-failure -R server"}},
	}
	if !isHardLeaf(cmake) {
		t.Fatal("ctest leaf must be hard")
	}
}

func TestLeafMaxStepsAdaptive(t *testing.T) {
	o := &Orchestrator{cfg: DefaultConfig()}
	o.cfg.MaxSteps = 20
	o.cfg.HardMaxSteps = 0 // use default 40
	easy := &models.TaskNode{ID: "a", Title: "add helper"}
	if got := o.leafMaxSteps(easy); got != 20 {
		t.Fatalf("easy MaxSteps = %d, want 20", got)
	}
	hard := &models.TaskNode{ID: "integrate", Title: "integrate server"}
	if got := o.leafMaxSteps(hard); got != DefaultHardMaxSteps {
		t.Fatalf("hard MaxSteps = %d, want %d", got, DefaultHardMaxSteps)
	}
	o.cfg.HardMaxSteps = 55
	if got := o.leafMaxSteps(hard); got != 55 {
		t.Fatalf("configured hard MaxSteps = %d, want 55", got)
	}
}

func TestLeafRequestTimeoutAdaptive(t *testing.T) {
	o := &Orchestrator{cfg: DefaultConfig()}
	o.cfg.RequestTimeout = 120 * time.Second
	easy := &models.TaskNode{ID: "a", Title: "helper"}
	if got := o.leafRequestTimeout(easy); got != 120*time.Second {
		t.Fatalf("easy timeout = %s", got)
	}
	hard := &models.TaskNode{ID: "integrate", Title: "server smoke"}
	if got := o.leafRequestTimeout(hard); got != DefaultHardRequestTimeout {
		t.Fatalf("hard timeout = %s, want %s", got, DefaultHardRequestTimeout)
	}
}

func TestEnsureOutputsBackfill(t *testing.T) {
	c := models.ContractSpec{}
	ensureOutputs(&c, "store", "KV store", "Implement pkg/store/store.go with Put/Get")
	if len(c.Outputs) != 1 || c.Outputs[0] != "pkg/store/store.go" {
		t.Fatalf("backfill from description = %#v", c.Outputs)
	}
	empty := models.ContractSpec{}
	ensureOutputs(&empty, "weak", "Weak leaf", "Do something vague")
	if len(empty.Outputs) != 1 || empty.Outputs[0] != "weak.out" {
		t.Fatalf("placeholder backfill = %#v", empty.Outputs)
	}
	kept := models.ContractSpec{Outputs: []string{"main.go", "", "main.go"}}
	ensureOutputs(&kept, "x", "x", "x")
	if len(kept.Outputs) != 1 || kept.Outputs[0] != "main.go" {
		t.Fatalf("dedupe = %#v", kept.Outputs)
	}
}
