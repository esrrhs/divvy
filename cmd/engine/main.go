package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/esrrhs/go_llm_engine/pkg/agent"
	"github.com/esrrhs/go_llm_engine/pkg/engine"
	"github.com/esrrhs/go_llm_engine/pkg/llm"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg := agent.DefaultConfig()

	fs := flag.NewFlagSet("go_llm_engine", flag.ContinueOnError)
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
	noStream := fs.Bool("no-stream", false, "disable SSE streaming")
	maxCost := fs.Float64("max-cost", cfg.MaxCost, "session cost ceiling in USD, incl. pre-resume spend (0 = unlimited)")
	budgetTokens := fs.Int("budget-tokens", cfg.BudgetTokens, "session token ceiling, incl. pre-resume spend (0 = unlimited)")
	pricing := fs.String("pricing", cfg.PricingJSON, "custom price table as JSON text or path to a JSON file ({\"model\":{\"input\":0.15,\"output\":0.6}} per 1M tokens)")
	resume := fs.Bool("resume", false, "resume a previous session")
	interactive := fs.Bool("interactive", false, "interactive REPL mode (multi-turn)")
	guided := fs.Bool("guided", false, "human-in-the-loop: plan, review/approve, then execute (mid-run plan edits and questions)")
	listSessions := fs.Bool("sessions", false, "list saved sessions and exit")
	status := fs.Bool("status", false, "print saved tree and exit")
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
	cfg.RetryMaxInterval = *retryMaxWait
	cfg.MaxDepth = *maxDepth
	cfg.MaxTokens = *maxTokens
	cfg.RequestTimeout = *timeout
	cfg.Parallel = *parallelN
	cfg.NativeTools = *native
	cfg.Stream = !*noStream
	cfg.Verbose = *verbose
	cfg.GitCommit = *gitCommit
	cfg.Strict = *strict
	cfg.MaxCost = *maxCost
	cfg.BudgetTokens = *budgetTokens
	cfg.PricingJSON = *pricing
	cfg.Goal = strings.TrimSpace(strings.Join(fs.Args(), " "))

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

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		gErr := agent.NewGuider(o, agent.NewStdioGuided(os.Stdin, os.Stdout)).Run(ctx)
		if gErr == agent.ErrPaused {
			log.Infof("paused. resume the guided flow with:\n  go_llm_engine -guided -resume -session %s -workdir %s",
				o.SessionID(), cfg.WorkDir)
			return nil
		}
		return gErr
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

	if *status {
		o, err := agent.Load(cfg, nil, log)
		if err != nil {
			return err
		}
		o.Status()
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	start := time.Now()
	if *plan {
		err = o.RunPlan(ctx)
	} else {
		err = o.Run(ctx)
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		log.Warnf("interrupted after %s. resume with:\n  go_llm_engine -resume -session %s -workdir %s",
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

const usageText = `go_llm_engine — divide-and-conquer coding agent for small/cheap models

Usage:
  go_llm_engine [flags] <goal>
  go_llm_engine -plan [flags] <goal>
  go_llm_engine -interactive [flags] [first message]
  go_llm_engine -guided [flags] <goal>
  go_llm_engine -resume [-session ID]
  go_llm_engine -status [-session ID]

Examples:
  export OPENAI_API_KEY=sk-...
  export OPENAI_BASE_URL=http://127.0.0.1:11434/v1
  export OPENAI_MODEL=qwen2.5-coder:14b

  go_llm_engine -workdir ./ws "用 Go 写一个 /health 返回 ok 的 HTTP 服务，并带单测"

  go_llm_engine -plan -workdir ./ws "目标"   # 只拆解，检查任务树
  go_llm_engine -plan -strict -workdir ./ws "目标"   # 拆解 + 严格检查（CI 友好）
  go_llm_engine -interactive -workdir .      # 交互式多轮开发（/help、/exit）
  go_llm_engine -resume -workdir ./ws        # 再执行

  go_llm_engine -max-cost 1 -budget-tokens 200000 -workdir ./ws "目标"
  go_llm_engine -parallel 4 -workdir ./ws "拆成多个独立模块的目标"
  go_llm_engine -git-commit -workdir ./ws "每个叶子一个提交，便于审计回滚"
  go_llm_engine -isolate -workdir ./ws "叶子失败不污染工作区"
  go_llm_engine -resume
  go_llm_engine -status

Environment:
  OPENAI_API_KEY / LLM_API_KEY
  OPENAI_BASE_URL / LLM_BASE_URL   (OpenAI-compatible, include /v1)
  OPENAI_MODEL / LLM_MODEL
  LLM_PRICING                      (custom price table JSON text or file path)

Flags:
`
