package agent

import (
	"strings"
	"testing"
)

// The goal-level acceptance command is a shell chain, not a bag of words.
// Reading it as one bag is what produced the blocker "goal acceptance
// references \"&&\" but no leaf declares it as an output" — a path nobody can
// produce, which burned the replan budget on every run that used a chain.

func TestAnalyzeAcceptance_ChainIsNotTokenSoup(t *testing.T) {
	a := analyzeAcceptance([]string{"go build -o app . && ./app test.json"})

	if len(a.required) != 0 {
		t.Fatalf("the binary is produced by the chain itself: required = %v", a.required)
	}
	if len(a.inputs) != 1 || a.inputs[0] != "test.json" {
		t.Fatalf("inputs = %v, want [test.json]", a.inputs)
	}
	if len(a.produced) == 0 {
		t.Fatal("the chain produces the binary; produced should not be empty")
	}
}

func TestAnalyzeAcceptance_ShellOperatorsAreNeverPaths(t *testing.T) {
	cmds := []string{
		"go build -o /tmp/app . && /tmp/app test.json",
		"go test ./... ; go build ./cmd/x",
		"go build . || go build ./cmd/y",
		"go build . | tee build.log",
	}
	for _, cmd := range cmds {
		a := analyzeAcceptance([]string{cmd})
		for _, p := range append(append([]string{}, a.required...), a.inputs...) {
			for _, op := range []string{"&&", "||", ";", "|"} {
				if p == op || strings.Contains(p, op) {
					t.Fatalf("cmd %q produced the operator as a path: %q", cmd, p)
				}
			}
		}
	}
}

// A path outside the workspace cannot be a tree deliverable, so it must not
// become a blocker. The real run wrote `/tmp/app` — no leaf can own it.
func TestAnalyzeAcceptance_OutsideWorkspaceIsNotADeliverable(t *testing.T) {
	a := analyzeAcceptance([]string{"go build . && /tmp/app test.json"})
	if len(a.required) != 0 {
		t.Fatalf("required = %v, want none (nothing can produce /tmp/app)", a.required)
	}
	if len(a.inputs) != 1 || a.inputs[0] != "test.json" {
		t.Fatalf("inputs = %v, want [test.json]", a.inputs)
	}
}

// `go build ./cmd/server` drops a binary named "server" in the cwd, so the
// `./server` that follows is not a dangling deliverable.
func TestAnalyzeAcceptance_GoBuildImplicitBinary(t *testing.T) {
	a := analyzeAcceptance([]string{"go build ./cmd/server && ./server --selftest"})
	if len(a.required) != 1 || !strings.Contains(a.required[0], "cmd/server") {
		t.Fatalf("required = %v, want just the package path", a.required)
	}
}

func TestAnalyzeAcceptance_RedirectTargetIsProducedNotRead(t *testing.T) {
	a := analyzeAcceptance([]string{`printf '{"name":"x"}' > test.json && go run . test.json`})
	if len(a.inputs) != 0 {
		t.Fatalf("the command writes the file itself: inputs = %v", a.inputs)
	}
}

// Source files are the output side's business. Tool reports (junit xml,
// coverage json) must not be mistaken for files that must pre-exist.
func TestAnalyzeAcceptance_OnlyDataFilesAreInputs(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want []string
	}{
		{"source file", "go run cmd/demo/main.go", nil},
		{"junit report", "pytest --junitxml report.xml", nil},
		{"coverage report", "go test -coverprofile=cover.out ./...", nil},
		{"data file", "go run . test.json", []string{"test.json"}},
		{"config file", "python3 -m app --config conf/app.yaml", []string{"conf/app.yaml"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := analyzeAcceptance([]string{c.cmd}).inputs
			if len(got) != len(c.want) {
				t.Fatalf("inputs(%q) = %v, want %v", c.cmd, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("inputs(%q) = %v, want %v", c.cmd, got, c.want)
				}
			}
		})
	}
}

// The extractor has to stay quiet on the ordinary shapes, or every plan that
// runs a real toolchain gets blocked by its own acceptance command.
func TestAnalyzeAcceptance_IgnoresNonFileTokens(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want []string
	}{
		{"go test", "go test ./...", nil},
		{"ctest", "ctest --test-dir build --output-on-failure -R add", nil},
		{"gradle test filter", `./gradlew test --tests "com.example.AppTest"`, nil},
		{"interpreted versions", "python3.11 -m pytest -q", nil},
		{"jar in build output", "java -jar build/libs/app.jar", nil},
		{"absolute path", "cat /etc/hostname", nil},
		{"env assignment", "MODE=release ./app", nil},
		{"url", "curl https://example.com/api.json", nil},
		{"wildcard", "cat fixtures/*.json", nil},
		{"redirect target", "echo hi > notes.txt", nil},
		{"real input", "go run . test.json", []string{"test.json"}},
		{"flag value", "python3 -m app --config conf/app.yaml", []string{"conf/app.yaml"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := analyzeAcceptance([]string{c.cmd}).inputs
			if len(got) != len(c.want) {
				t.Fatalf("inputs(%q) = %v, want %v", c.cmd, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("inputs(%q) = %v, want %v", c.cmd, got, c.want)
				}
			}
		})
	}
}
