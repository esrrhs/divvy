package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/cost"
	"github.com/esrrhs/divvy/pkg/llm"
	"github.com/esrrhs/divvy/pkg/models"
	"github.com/esrrhs/divvy/pkg/tools"
)

// Bench loops. Tree is the task-tree kernel the harness was built for; direct
// is the persistent tool loop the web UI defaults to. They are measured the
// same way on purpose: identical corpus, identical workspace seeding, identical
// acceptance command — only the loop differs.
const (
	loopTree   = "tree"
	loopDirect = "direct"
)

// acceptTimeout bounds the harness-run acceptance command. It is deliberately
// separate from the model's own step timeouts: the harness is allowed to wait
// longer than a single tool call would.
const acceptTimeout = 3 * time.Minute

// normalizeLoop resolves -loop, accepting "guided" as a synonym for the task
// tree because that is what the engine calls it internally.
func normalizeLoop(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", loopTree, agent.AgentLoopGuided:
		return loopTree, nil
	case loopDirect:
		return loopDirect, nil
	}
	return "", fmt.Errorf("-loop must be %q or %q, got %q", loopTree, loopDirect, s)
}

// benchConfig is one bench invocation's settings, shared by every goal run.
type benchConfig struct {
	BaseURL   string
	Model     string
	APIKey    string
	ExtraJSON string

	// Loop is loopTree or loopDirect; empty means tree.
	Loop string

	MaxSteps   int
	MaxRetries int
	Parallel   int
	Isolate    bool
	GitCommit  bool
	Timeout    time.Duration

	// Root is the directory that holds one subdirectory per goal run (the
	// seeded workspace plus the run's own .divvy session data). Workspaces
	// are kept after the run for post-mortem inspection.
	Root string

	Pricing   *cost.Pricing
	newClient func(agent.Config) llm.Client // nil builds the real client
}

// RunResult is one goal run's outcome and cost accounting.
type RunResult struct {
	Goal   string `json:"goal"`
	Repeat int    `json:"repeat"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`

	WallMS int64 `json:"wall_ms"`

	LLMCalls         int     `json:"llm_calls"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`

	Leaves     int `json:"leaves"`
	LeavesDone int `json:"leaves_done"`

	VerifyFail int `json:"verify_fail"`
	Retries    int `json:"retries"`
	Stalls     int `json:"stalls"`
	Resplits   int `json:"resplits"`
	GiveUps    int `json:"give_ups"`

	// Loop records which engine loop produced this row so a report that mixes
	// -loop values is still readable.
	Loop string `json:"loop,omitempty"`

	// Accept* describe the harness-run acceptance of direct-mode runs. Tree
	// mode decides completion through the plan's own DoD, so these stay false
	// there; AcceptRan=false on a direct run means the host cannot execute
	// commands at all and the loop's own verdict is all we have.
	AcceptRan bool   `json:"accept_ran"`
	AcceptOK  bool   `json:"accept_ok"`
	AcceptOut string `json:"accept_output,omitempty"`

	Workdir   string `json:"workdir"`
	SessionID string `json:"session_id"`
}

// Aggregate rolls results up for the bench-level verdict.
type Aggregate struct {
	Runs        int     `json:"runs"`
	OK          int     `json:"ok"`
	WallMS      int64   `json:"wall_ms"`
	LLMCalls    int     `json:"llm_calls"`
	TotalTokens int     `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
	VerifyFail  int     `json:"verify_fail"`
	Retries     int     `json:"retries"`
	Stalls      int     `json:"stalls"`
	Resplits    int     `json:"resplits"`
	SuccessRate float64 `json:"success_rate"`
}

func aggregate(rs []RunResult) Aggregate {
	var a Aggregate
	for _, r := range rs {
		a.Runs++
		if r.OK {
			a.OK++
		}
		a.WallMS += r.WallMS
		a.LLMCalls += r.LLMCalls
		a.TotalTokens += r.TotalTokens
		a.CostUSD += r.CostUSD
		a.VerifyFail += r.VerifyFail
		a.Retries += r.Retries
		a.Stalls += r.Stalls
		a.Resplits += r.Resplits
	}
	if a.Runs > 0 {
		a.SuccessRate = float64(a.OK) / float64(a.Runs)
	}
	return a
}

// leafSteps resolves the per-leaf budget: a goal that pins its own wins, since
// that is what makes it a decomposition test rather than another one-context
// task.
func (g Goal) leafSteps(benchDefault int) int {
	if g.LeafSteps > 0 {
		return g.LeafSteps
	}
	return benchDefault
}

// seedWorkspace materializes the goal's seed files, initializes the Go
// module, and makes an initial git commit so -git-commit has a baseline and
// a post-mortem `git diff` shows exactly what the model changed.
func (g Goal) seedWorkspace(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for rel, content := range g.Seed {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return fmt.Errorf("seed %s: %w", rel, err)
		}
	}
	if g.Module != "" {
		if out, err := runCmd(dir, "go", "mod", "init", g.Module); err != nil {
			return fmt.Errorf("go mod init: %v: %s", err, out)
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "bench@divvy.local"},
		{"config", "user.name", "divvy-bench"},
		{"add", "-A"},
		{"commit", "-q", "-m", "seed"},
	} {
		if out, err := runCmd(dir, "git", args...); err != nil {
			return fmt.Errorf("git %v: %v: %s", args, err, out)
		}
	}
	return nil
}

func runCmd(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runGoal executes one goal once in a fresh workspace and collects the
// outcome. It never panics on run failure: a failed goal is a data point,
// not a harness error.
func runGoal(ctx context.Context, g Goal, repeat int, bc benchConfig) RunResult {
	loop, err := normalizeLoop(bc.Loop)
	if err != nil {
		return RunResult{Goal: g.Name, Repeat: repeat, Error: err.Error()}
	}
	res := RunResult{Goal: g.Name, Repeat: repeat, Loop: loop}
	res.Workdir = filepath.Join(bc.Root, fmt.Sprintf("%s-r%d", g.Name, repeat))

	if err := g.seedWorkspace(res.Workdir); err != nil {
		res.Error = "seed workspace: " + err.Error()
		return res
	}

	cfg := agent.DefaultConfig()
	cfg.Goal = g.Prompt
	cfg.WorkDir = res.Workdir
	cfg.DataDir = filepath.Join(res.Workdir, ".divvy")
	cfg.Model = bc.Model
	cfg.BaseURL = bc.BaseURL
	cfg.APIKey = bc.APIKey
	cfg.ExtraJSON = bc.ExtraJSON
	cfg.Stream = false // token streaming adds noise, not signal, to a bench
	if loop == loopDirect {
		cfg.AgentLoop = agent.AgentLoopDirect
	}
	if steps := g.leafSteps(bc.MaxSteps); steps > 0 {
		cfg.MaxSteps = steps
	}
	if bc.MaxRetries > 0 {
		cfg.MaxRetries = bc.MaxRetries
	}
	if bc.Parallel > 0 {
		cfg.Parallel = bc.Parallel
	}
	cfg.Isolate = bc.Isolate
	cfg.GitCommit = bc.GitCommit

	newClient := bc.newClient
	if newClient == nil {
		newClient = func(cfg agent.Config) llm.Client {
			c := llm.NewOpenAIClient(cfg.APIKey, cfg.BaseURL, cfg.RequestTimeout)
			c.ExtraJSON = cfg.ExtraJSON
			c.MaxBackoff = cfg.RetryMaxInterval
			return c
		}
	}

	o, err := agent.NewFromGoal(cfg, newClient(cfg), agent.SilentLogger())
	if err != nil {
		res.Error = "create orchestrator: " + err.Error()
		return res
	}
	defer o.Close()
	res.SessionID = o.SessionID()

	runCtx, cancel := context.WithTimeout(ctx, bc.Timeout)
	defer cancel()

	start := time.Now()
	var runErr error
	if loop == loopDirect {
		// No task tree and no plan: the model drives one tool transcript.
		runErr = agent.NewDirect(o, nil).Run(runCtx)
	} else {
		runErr = o.Run(runCtx)
	}
	res.WallMS = time.Since(start).Milliseconds()

	usage := o.Tree().TotalTokenUsage()
	res.LLMCalls = usage.Calls
	res.PromptTokens = usage.PromptTokens
	res.CompletionTokens = usage.CompletionTokens
	res.TotalTokens = usage.TotalTokens
	if bc.Pricing != nil {
		if p, ok := bc.Pricing.PriceFor(bc.Model); ok && !p.Zero() {
			res.CostUSD = p.Cost(usage.PromptTokens, usage.CompletionTokens)
		}
	}
	res.LeavesDone, res.Leaves, _ = o.Tree().GetLeafProgress()
	if summary, serr := agent.SummarizeEvents(o.EventPath()); serr == nil {
		res.VerifyFail = summary.VerifyFail
		res.Retries = summary.Retries
		res.Stalls = summary.Stalls
		res.Resplits = summary.Resplits
		res.GiveUps = summary.GiveUps
	}

	switch {
	case runErr != nil:
		res.Error = oneLine(runErr.Error())
	case loop == loopDirect:
		// Direct mode has no contract to satisfy, so the harness supplies the
		// verdict: run the same acceptance command the tree goals declare.
		ok, ran, out := runAcceptance(runCtx, g, res.Workdir)
		res.AcceptRan, res.AcceptOK, res.AcceptOut = ran, ok, out
		switch {
		case !ran:
			// The host cannot execute commands (no isolation backend), so the
			// loop's own finish gate is the only verdict available. Say so in
			// the row instead of pretending the goal was verified.
			res.OK = true
			res.AcceptOut = out
		case ok:
			res.OK = true
		default:
			res.Error = "acceptance failed: " + oneLine(out)
		}
	case rootCompleted(o):
		res.OK = true
	default:
		res.Error = "run returned no error but the root goal is not COMPLETED"
	}
	// A pass that required editing the test which defines the task is not a
	// pass. Checked after the verdict so a run that already failed keeps its
	// own error instead of being explained away by this one.
	if res.OK {
		if viol := guardViolation(res.Workdir, g); viol != "" {
			res.OK = false
			res.Error = "protected seed file: " + viol
		}
	}
	return res
}

// guardViolation reports the first protected seed file whose bytes changed (or
// that disappeared). Goals whose verdict rests on a seeded test carry that file
// in Guard: without the check, a model can satisfy `go test ./...` by relaxing
// the assertion, and the bench would report a capability that never existed.
func guardViolation(workdir string, g Goal) string {
	for _, rel := range g.Guard {
		want, ok := g.Seed[rel]
		if !ok {
			continue
		}
		got, err := os.ReadFile(filepath.Join(workdir, filepath.FromSlash(rel)))
		if err != nil {
			return rel + " is missing"
		}
		if string(got) != want {
			return rel + " was modified"
		}
	}
	return ""
}

// runAcceptance runs the goal's acceptance command against the finished
// workspace. ran=false means the command could not be attempted at all (no
// isolation backend, or the goal declares no acceptance), which is different
// from "ran and failed".
func runAcceptance(ctx context.Context, g Goal, workdir string) (ok, ran bool, out string) {
	if strings.TrimSpace(g.Accept) == "" {
		return false, false, "goal declares no acceptance command"
	}
	sb, err := tools.NewSandbox(workdir)
	if err != nil {
		return false, false, "sandbox: " + err.Error()
	}
	tools.ApplyRuntimePolicy(sb)
	if reason, disabled := sb.ToolDisabled(tools.ToolRunBash); disabled {
		return false, false, reason
	}
	actx, cancel := context.WithTimeout(ctx, acceptTimeout)
	defer cancel()
	res, err := sb.RunBash(actx, g.Accept, acceptTimeout)
	if err != nil {
		return false, true, oneLine(err.Error())
	}
	if res.ExitCode == 0 && !res.TimedOut {
		return true, true, ""
	}
	detail := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	if detail == "" {
		detail = fmt.Sprintf("exit %d", res.ExitCode)
	}
	return false, true, clipAcceptOutput(detail)
}

// acceptOutputCap keeps a failing acceptance from dumping a whole build log
// into the report.
const acceptOutputCap = 600

func clipAcceptOutput(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > acceptOutputCap {
		s = s[:acceptOutputCap] + "..."
	}
	return s
}

// rootCompleted reports the goal-level verdict, mirroring
// engine.Scheduler.IsComplete: the root must be COMPLETED and, for a
// compound root, its goal-level acceptance DoD must have passed
// (IntegrationVerified). For an atomic goal the root IS a leaf and its own
// DoD is the acceptance, so COMPLETED alone suffices — do not require
// IntegrationVerified there, it is deliberately never set for leaf roots.
func rootCompleted(o *agent.Orchestrator) bool {
	root := o.Tree().GetRoot()
	if root == nil || root.State != models.TaskStateCompleted {
		return false
	}
	return root.Type == models.NodeTypeLeaf || root.IntegrationVerified
}

// oneLine collapses an error message onto a single bounded line.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 90 {
		s = s[:90] + "..."
	}
	return s
}
