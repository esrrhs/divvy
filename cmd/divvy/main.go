package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/esrrhs/divvy/pkg/agent"
	"github.com/esrrhs/divvy/pkg/engine"
	"github.com/esrrhs/divvy/pkg/llm"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg := agent.DefaultConfig()

	fs := flag.NewFlagSet("divvy", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usageText)
		fs.PrintDefaults()
	}

	workdir := fs.String("workdir", cfg.WorkDir, "workspace directory for generated code")
	datadir := fs.String("datadir", cfg.DataDir, "session storage directory")
	session := fs.String("session", "", "session id (default: timestamp, or LATEST on resume)")
	model := fs.String("model", cfg.Model, "model name")
	baseURL := fs.String("base-url", cfg.BaseURL, "OpenAI-compatible API base URL")
	apiKey := fs.String("api-key", cfg.APIKey, "API key (or OPENAI_API_KEY / LLM_API_KEY)")
	extra := fs.String("extra", "", "extra JSON merged into chat request body")
	maxSteps := fs.Int("max-steps", cfg.MaxSteps, "max tool calls per leaf attempt")
	maxRetries := fs.Int("max-retries", cfg.MaxRetries, "max verify retries per leaf (0 = unlimited)")
	maxStall := fs.Int("max-stall", cfg.MaxStall, "give up on a leaf after this many identical failures in a row (0 = never)")
	maxElapsed := fs.Duration("max-elapsed", cfg.MaxElapsed, "wall-clock budget for one leaf across all attempts (0 = unlimited)")
	retryMaxWait := fs.Duration("retry-max-wait", cfg.RetryMaxInterval, "exponential backoff cap between retries")
	maxDepth := fs.Int("max-depth", cfg.MaxDepth, "max decomposition depth")
	maxTokens := fs.Int("max-tokens", cfg.MaxTokens, "max completion tokens")
	timeout := fs.Duration("timeout", cfg.RequestTimeout, "per-request timeout")
	parallelN := fs.Int("parallel", cfg.Parallel, "leaves to execute concurrently (only safe when leaves touch different files, unless -isolate)")
	isolate := fs.Bool("isolate", false, "run each leaf in a workspace mirror, merge only on success (default: on when -parallel > 1)")
	plan := fs.Bool("plan", false, "decompose into a task tree and exit without executing (run later with -resume)")
	strict := fs.Bool("strict", false, "with -plan: exit non-zero when the plan check reports warnings")
	gitCommit := fs.Bool("git-commit", false, "git commit each leaf's merged changes (workdir must be a git repo)")
	native := fs.Bool("native-tools", false, "use OpenAI tool_calls instead of JSON actions")
	web := fs.Bool("web", false, "enable outbound web_search/web_fetch/http_request for leaves (offline by default)")
	searchURL := fs.String("search-url", "", "search endpoint template with {query} (default: DuckDuckGo lite; SearXNG: http://host/search?q={query}&format=json)")
	browser := fs.Bool("browser", false, "enable headless Chrome tools for leaves (requires Chrome/Chromium)")
	noStream := fs.Bool("no-stream", false, "disable SSE streaming")
	maxCost := fs.Float64("max-cost", cfg.MaxCost, "session cost ceiling in USD, incl. pre-resume spend (0 = unlimited)")
	budgetTokens := fs.Int("budget-tokens", cfg.BudgetTokens, "session token ceiling, incl. pre-resume spend (0 = unlimited)")
	pricing := fs.String("pricing", cfg.PricingJSON, "custom price table as JSON text or path to a JSON file ({\"model\":{\"input\":0.15,\"output\":0.6}} per 1M tokens)")
	resume := fs.Bool("resume", false, "resume a previous session")
	serve := fs.Bool("serve", false, "start the local web UI server (loopback HTTP + SSE) and open the browser")
	port := fs.Int("port", 0, "with -serve: port to listen on (0 = pick a free port)")
	noOpen := fs.Bool("no-open", false, "with -serve: do not open a browser automatically")
	interactive := fs.Bool("interactive", false, "interactive REPL mode (multi-turn)")
	guided := fs.Bool("guided", false, "human-in-the-loop: plan, review/approve, then execute (mid-run plan edits and questions)")
	listSessions := fs.Bool("sessions", false, "list saved sessions and exit")
	status := fs.Bool("status", false, "print saved tree and exit")
	report := fs.Bool("report", false, "print a post-mortem summary of a saved session and exit (-session or LATEST)")
	prune := fs.Bool("prune", false, "delete old saved sessions, keeping the newest -keep ones (LATEST is never removed)")
	keep := fs.Int("keep", 5, "with -prune: how many recent sessions to keep")
	dryRun := fs.Bool("dry-run", false, "with -prune: only show what would be deleted")
	showLog := fs.Bool("log", false, "print this session's run log and exit (-session or LATEST)")
	showEvents := fs.Bool("events", false, "print this session's JSONL event stream and exit (-session or LATEST)")
	verbose := fs.Bool("v", false, "verbose logs (raw model snippets, tool output)")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}

	cfg.WorkDir = *workdir
	cfg.DataDir = *datadir
	cfg.SessionID = *session
	cfg.Model = *model
	cfg.BaseURL = strings.TrimRight(*baseURL, "/")
	cfg.APIKey = *apiKey
	cfg.ExtraJSON = *extra
	cfg.MaxSteps = *maxSteps
	cfg.MaxRetries = *maxRetries
	cfg.MaxStall = *maxStall
	cfg.MaxElapsed = *maxElapsed
	cfg.RetryMaxInterval = *retryMaxWait
	cfg.MaxDepth = *maxDepth
	cfg.MaxTokens = *maxTokens
	cfg.RequestTimeout = *timeout
	cfg.Parallel = *parallelN
	cfg.NativeTools = *native
	cfg.Stream = !*noStream
	cfg.Verbose = *verbose
	cfg.WebEnabled = *web
	cfg.SearchURL = *searchURL
	cfg.BrowserEnabled = *browser
	cfg.GitCommit = *gitCommit
	cfg.Strict = *strict
	cfg.MaxCost = *maxCost
	cfg.BudgetTokens = *budgetTokens
	cfg.PricingJSON = *pricing
	cfg.Goal = strings.TrimSpace(strings.Join(fs.Args(), " "))

	// Interactive sessions don't persist a task tree; unless the user set
	// -datadir explicitly, keep its storage in a temp dir instead of
	// creating .divvy inside the live workspace.
	if *interactive {
		explicitData := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "datadir" {
				explicitData = true
			}
		})
		if !explicitData {
			d, merr := os.MkdirTemp("", "divvy_repl_")
			if merr != nil {
				return merr
			}
			cfg.DataDir = d
		}
	}

	// -isolate defaults to on whenever several leaves may run at once.
	explicitIsolate := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "isolate" {
			explicitIsolate = true
		}
	})
	if !explicitIsolate {
		cfg.Isolate = cfg.Parallel > 1
	} else {
		cfg.Isolate = *isolate
	}

	absWork, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return err
	}
	cfg.WorkDir = absWork

	log := agent.NewLogger(cfg.Verbose)

	if *interactive && *guided {
		return fmt.Errorf("-interactive and -guided are mutually exclusive; choose one")
	}

	if *serve {
		if *interactive || *guided || *plan || *resume {
			return fmt.Errorf("-serve is mutually exclusive with -interactive/-guided/-plan/-resume; drive runs from the web UI instead")
		}
		if fs.NArg() > 0 {
			return fmt.Errorf("-serve takes no goal argument; create sessions in the web UI")
		}
		for _, on := range []struct {
			name string
			v    bool
		}{
			{"sessions", *listSessions}, {"status", *status}, {"report", *report},
			{"prune", *prune}, {"log", *showLog}, {"events", *showEvents},
		} {
			if on.v {
				return fmt.Errorf("-serve is mutually exclusive with -%s", on.name)
			}
		}
		return runServe(cfg, log, *port, *noOpen)
	}

	if *interactive {
		if cfg.RequiresAPIKey() && cfg.APIKey == "" {
			return fmt.Errorf("missing API key: set OPENAI_API_KEY or pass -api-key")
		}
		client := llm.NewOpenAIClient(cfg.APIKey, cfg.BaseURL, cfg.RequestTimeout)
		client.ExtraJSON = cfg.ExtraJSON
		client.MaxBackoff = cfg.RetryMaxInterval

		repl, err := agent.NewREPL(cfg, client)
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return repl.Run(ctx, agent.NewStdioREPL(os.Stdin, os.Stdout))
	}

	if *guided {
		if cfg.RequiresAPIKey() && cfg.APIKey == "" {
			return fmt.Errorf("missing API key: set OPENAI_API_KEY or pass -api-key")
		}
		client := llm.NewOpenAIClient(cfg.APIKey, cfg.BaseURL, cfg.RequestTimeout)
		client.ExtraJSON = cfg.ExtraJSON
		client.MaxBackoff = cfg.RetryMaxInterval

		var o *agent.Orchestrator
		if *resume {
			o, err = agent.Load(cfg, client, log)
		} else {
			o, err = agent.NewFromGoal(cfg, client, log)
		}
		if err != nil {
			return err
		}
		defer o.Close()

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		gErr := agent.NewGuider(o, agent.NewStdioGuided(os.Stdin, os.Stdout)).Run(ctx)
		if gErr == agent.ErrPaused {
			log.Infof("paused. resume the guided flow with:\n  divvy -guided -resume -session %s -workdir %s",
				o.SessionID(), cfg.WorkDir)
			return nil
		}
		if gErr == context.Canceled || gErr == context.DeadlineExceeded {
			log.Warnf("interrupted. resume the guided flow with:\n  divvy -guided -resume -session %s -workdir %s",
				o.SessionID(), cfg.WorkDir)
			return nil
		}
		return gErr
	}

	if *showLog || *showEvents {
		return printSessionArtifact(cfg, *showEvents)
	}

	if *listSessions {
		storage, err := engine.NewStorage(cfg.DataDir)
		if err != nil {
			return err
		}
		sessions, err := storage.ListSessions()
		if err != nil {
			return err
		}
		if len(sessions) == 0 {
			fmt.Printf("no saved sessions in %s\n", cfg.DataDir)
			return nil
		}
		fmt.Printf("%-28s %-9s %-8s %s\n", "SESSION", "STATE", "LEAVES", "UPDATED  GOAL")
		for _, s := range sessions {
			goal := s.Goal
			if len(goal) > 60 {
				goal = goal[:60] + "..."
			}
			fmt.Printf("%-28s %-9s %-8s %s  %s\n",
				s.ID, s.RootState, fmt.Sprintf("%d/%d", s.LeavesDone, s.LeavesAll),
				s.UpdatedAt.Format("01-02 15:04"), goal)
		}
		return nil
	}

	if *prune {
		return pruneSessions(cfg, *keep, *dryRun)
	}

	if *status {
		o, err := agent.Load(cfg, nil, log)
		if err != nil {
			return err
		}
		o.Status()
		return nil
	}

	if *report {
		o, err := agent.Load(cfg, nil, log)
		if err != nil {
			return err
		}
		fmt.Print(o.Report())
		return nil
	}

	if !*resume && cfg.Goal == "" {
		fs.Usage()
		return fmt.Errorf("goal is required (or pass -resume)")
	}
	if cfg.RequiresAPIKey() && cfg.APIKey == "" {
		return fmt.Errorf("missing API key: set OPENAI_API_KEY or pass -api-key")
	}
	if cfg.MaxCost < 0 {
		return fmt.Errorf("-max-cost must be >= 0")
	}
	if cfg.BudgetTokens < 0 {
		return fmt.Errorf("-budget-tokens must be >= 0")
	}
	if cfg.MaxElapsed < 0 {
		return fmt.Errorf("-max-elapsed must be >= 0")
	}
	if cfg.MaxRetries < 0 {
		return fmt.Errorf("-max-retries must be >= 0")
	}
	if cfg.MaxStall < 0 {
		return fmt.Errorf("-max-stall must be >= 0")
	}

	client := llm.NewOpenAIClient(cfg.APIKey, cfg.BaseURL, cfg.RequestTimeout)
	client.ExtraJSON = cfg.ExtraJSON
	client.MaxBackoff = cfg.RetryMaxInterval

	var o *agent.Orchestrator
	if *resume {
		o, err = agent.Load(cfg, client, log)
	} else {
		o, err = agent.NewFromGoal(cfg, client, log)
	}
	if err != nil {
		return err
	}
	defer o.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	start := time.Now()
	if *plan {
		err = o.RunPlan(ctx)
	} else {
		err = o.Run(ctx)
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		log.Warnf("interrupted after %s. resume with:\n  divvy -resume -session %s -workdir %s",
			time.Since(start).Truncate(time.Second), o.SessionID(), cfg.WorkDir)
		return err
	}
	if err != nil {
		log.Errorf("%v", err)
		log.Infof("session saved: %s  (resume with -resume -session %s)", o.SessionID(), o.SessionID())
		return err
	}
	log.Okf("done in %s", time.Since(start).Truncate(time.Millisecond))
	return nil
}

// pruneSessions drops the oldest saved sessions and their logs/event streams.
// It never touches the LATEST session, so a bare `-resume` keeps working.
func pruneSessions(cfg agent.Config, keep int, dryRun bool) error {
	storage, err := engine.NewStorage(cfg.DataDir)
	if err != nil {
		return err
	}
	verb := "deleted"
	plan, err := storage.PlanPrune(keep)
	if err != nil {
		return err
	}
	if dryRun {
		verb = "would delete"
	} else {
		plan, err = storage.PruneSessions(keep)
		if err != nil {
			return err
		}
	}
	if len(plan.Removed) == 0 {
		fmt.Printf("nothing to prune: %d session(s) in %s, keeping %d\n", len(plan.Kept), cfg.DataDir, keep)
		return nil
	}
	for _, art := range plan.Removed {
		for _, p := range art.Paths {
			fmt.Printf("%s %s (%s)\n", verb, p, art.ID)
		}
	}
	fmt.Printf("%s %d session(s), %s; %d kept\n", verb, len(plan.Removed), humanBytes(plan.Bytes), len(plan.Kept))
	return nil
}

// humanBytes renders a byte count compactly for the prune summary.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// printSessionArtifact streams one session's run log (events=false) or JSONL
// event stream (events=true) to stdout. The session is -session or LATEST.
func printSessionArtifact(cfg agent.Config, events bool) error {
	sessionID := cfg.SessionID
	if sessionID == "" {
		data, err := os.ReadFile(filepath.Join(cfg.DataDir, "LATEST"))
		if err != nil {
			return fmt.Errorf("no -session given and no LATEST pointer in %s", cfg.DataDir)
		}
		sessionID = strings.TrimSpace(string(data))
	}

	var rel, kind string
	if events {
		rel = filepath.Join("events", sessionID+".jsonl")
		kind = "event stream"
	} else {
		rel = filepath.Join("logs", sessionID+".log")
		kind = "run log"
	}
	path := filepath.Join(cfg.DataDir, rel)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no %s for session %s (%s does not exist); list sessions with -sessions", kind, sessionID, path)
		}
		return err
	}
	defer f.Close()
	_, err = io.Copy(os.Stdout, f)
	return err
}

const usageText = `divvy — divide-and-conquer coding agent for small/cheap models

Usage:
  divvy [flags] <goal>
  divvy -plan [flags] <goal>
  divvy -interactive [flags] [first message]
  divvy -guided [flags] <goal>
  divvy -serve [-port 0] [-no-open] [flags]   # 本机 Web 界面（loopback，带随机 token）
  divvy -resume [-session ID]
  divvy -status [-session ID]
  divvy -report [-session ID]   # 事后复盘：进度、成本、重试/停滞、失败原因
  divvy -prune -keep 5          # 清理旧会话（-dry-run 先看清单）
  divvy -log [-session ID]     # 查看运行日志（时间戳文本）
  divvy -events [-session ID]  # 查看结构化事件流（JSONL，便于 jq/grep）

Examples:
  export OPENAI_API_KEY=sk-...
  export OPENAI_BASE_URL=http://127.0.0.1:11434/v1
  export OPENAI_MODEL=qwen2.5-coder:14b

  divvy -workdir ./ws "用 Go 写一个 /health 返回 ok 的 HTTP 服务，并带单测"

  divvy -plan -workdir ./ws "目标"   # 只拆解，检查任务树
  divvy -plan -strict -workdir ./ws "目标"   # 拆解 + 严格检查（CI 友好）
  divvy -interactive -workdir .      # 交互式多轮开发（/help、/exit）
  divvy -resume -workdir ./ws        # 再执行

  divvy -max-cost 1 -budget-tokens 200000 -workdir ./ws "目标"
  divvy -max-retries 0 -max-elapsed 30m -max-stall 3 -workdir ./ws "目标"   # 无限重试时的两道闸门
  divvy -report                   # 事后看这次跑得怎么样
  divvy -prune -keep 5 -dry-run   # 旧会话占空间时的清理清单
  divvy -parallel 4 -workdir ./ws "拆成多个独立模块的目标"
  divvy -git-commit -workdir ./ws "每个叶子一个提交，便于审计回滚"
  divvy -isolate -workdir ./ws "叶子失败不污染工作区"
  divvy -resume
  divvy -status
  divvy -log                      # 出问题时回溯最近一次运行
  divvy -events | jq 'select(.kind=="verify" and .ok==false)'

Environment:
  OPENAI_API_KEY / LLM_API_KEY
  OPENAI_BASE_URL / LLM_BASE_URL   (OpenAI-compatible, include /v1)
  OPENAI_MODEL / LLM_MODEL
  LLM_PRICING                      (custom price table JSON text or file path)

Flags:
`
