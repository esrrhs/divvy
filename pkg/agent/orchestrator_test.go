package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
)

func TestOrchestrator_AtomicLeafE2E(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	var workerN int
	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: `{
  "is_atomic": true,
  "reason": "single package",
  "contract": {"inputs": [], "outputs": ["greet.go", "greet_test.go", "go.mod"], "dependencies": [], "constraints": []},
  "dod": {"description": "tests pass", "commands": ["go test ./..."], "timeout_sec": 60},
  "subtasks": []
}`}, nil
			}
			workerN++
			switch workerN {
			case 1:
				return jsonAction("write_file", map[string]string{
					"path":    "go.mod",
					"content": "module greet\n\ngo 1.22\n",
				}), nil
			case 2:
				return jsonAction("write_file", map[string]string{
					"path":    "greet.go",
					"content": "package greet\n\nfunc Hello() string { return \"hello\" }\n",
				}), nil
			case 3:
				return jsonAction("write_file", map[string]string{
					"path":    "greet_test.go",
					"content": "package greet\n\nimport \"testing\"\n\nfunc TestHello(t *testing.T) {\n\tif Hello() != \"hello\" {\n\t\tt.Fatal(Hello())\n\t}\n}\n",
				}), nil
			default:
				return jsonAction("finish", map[string]string{"summary": "package greet ready"}), nil
			}
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_atomic"
	cfg.Goal = "Create a Go package greet with Hello() string returning hello, plus a unit test"
	cfg.Stream = false
	cfg.MaxRetries = 1
	cfg.Verbose = false

	log := SilentLogger()
	o, err := NewFromGoal(cfg, client, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !o.sched.IsComplete() {
		t.Fatalf("not complete:\n%s", o.tree.RenderVisualTree())
	}

	raw, err := os.ReadFile(filepath.Join(work, "greet.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "func Hello") {
		t.Fatalf("missing Hello: %s", raw)
	}

	root := o.tree.GetRoot()
	if root.Type != models.NodeTypeLeaf {
		t.Fatalf("root type %s", root.Type)
	}
}

func TestOrchestrator_TwoLeafDependency(t *testing.T) {
	work := t.TempDir()
	data := t.TempDir()

	var workerN int
	client := &llm.ScriptedClient{
		Handle: func(ctx context.Context, req llm.Request) (*llm.Response, error) {
			sys := ""
			if len(req.Messages) > 0 {
				sys = req.Messages[0].Content
			}
			if strings.Contains(sys, architectMarker) {
				return &llm.Response{Content: `{
  "is_atomic": false,
  "reason": "split code and test",
  "subtasks": [
    {
      "id": "impl",
      "title": "Implement Hello",
      "description": "Write go.mod and greet.go with Hello() string",
      "type": "LEAF",
      "contract": {"outputs": ["go.mod", "greet.go"]},
      "dod": {"commands": ["test -f greet.go"]}
    },
    {
      "id": "test",
      "title": "Write unit test",
      "description": "Write greet_test.go",
      "type": "LEAF",
      "contract": {"outputs": ["greet_test.go"], "dependencies": ["impl"]},
      "dod": {"commands": ["go test ./..."]}
    }
  ]
}`}, nil
			}
			workerN++
			switch workerN {
			case 1:
				return jsonAction("write_file", map[string]string{"path": "go.mod", "content": "module greet\n\ngo 1.22\n"}), nil
			case 2:
				return jsonAction("write_file", map[string]string{"path": "greet.go", "content": "package greet\n\nfunc Hello() string { return \"hello\" }\n"}), nil
			case 3:
				return jsonAction("finish", map[string]string{"summary": "impl done"}), nil
			case 4:
				return jsonAction("write_file", map[string]string{"path": "greet_test.go", "content": "package greet\n\nimport \"testing\"\n\nfunc TestHello(t *testing.T) {\n\tif Hello() != \"hello\" { t.Fatal(Hello()) }\n}\n"}), nil
			default:
				return jsonAction("finish", map[string]string{"summary": "test done"}), nil
			}
		},
	}

	cfg := DefaultConfig()
	cfg.WorkDir = work
	cfg.DataDir = data
	cfg.SessionID = "test_two"
	cfg.Goal = "greet package"
	cfg.Stream = false
	cfg.MaxRetries = 1

	o, err := NewFromGoal(cfg, client, SilentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !o.sched.IsComplete() {
		t.Fatalf("not complete:\n%s", o.tree.RenderVisualTree())
	}
}

func jsonAction(name string, args map[string]string) *llm.Response {
	payload := map[string]any{
		"thought": "x",
		"action":  name,
		"args":    args,
	}
	b, _ := json.Marshal(payload)
	return &llm.Response{Content: string(b)}
}
