package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/esrrhs/divvy/pkg/cost"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "divvy-bench:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("divvy-bench", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: divvy-bench [flags]\n\n"+
			"Runs the fixed weak-model goal corpus against an OpenAI-compatible\n"+
			"endpoint and reports success/tokens/time per goal.\n\n")
		fs.PrintDefaults()
	}

	baseURL := fs.String("base-url", "http://127.0.0.1:11434/v1", "OpenAI-compatible endpoint (default: local Ollama)")
	model := fs.String("model", "qwen3.8:27b", "model name")
	apiKey := fs.String("api-key", "", "API key (local endpoints usually need none)")
	extra := fs.String("extra", "", `extra request JSON, e.g. '{"reasoning_effort":"none"}'`)
	goals := fs.String("goals", "", "comma-separated corpus names (default: all; -list shows them)")
	list := fs.Bool("list", false, "list the corpus and exit")
	root := fs.String("root", "", "workspace root (default: bench-runs/<timestamp>)")
	out := fs.String("out", "", "JSON report path (default: <root>/report.json)")
	repeat := fs.Int("repeat", 1, "run the whole corpus this many times")
	timeout := fs.Duration("timeout", 20*time.Minute, "per-goal wall-clock budget")
	maxSteps := fs.Int("max-steps", 15, "worker tool-call budget per leaf")
	maxRetries := fs.Int("max-retries", 2, "leaf retry budget")
	parallel := fs.Int("parallel", 1, "concurrent leaves per run")
	isolate := fs.Bool("isolate", true, "run leaves in isolated mirrors, merge on success")
	gitCommit := fs.Bool("git-commit", true, "commit each leaf's changes (requires git)")
	loop := fs.String("loop", loopTree, "engine loop to measure: tree (plan + verify) or direct (persistent tool loop)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *list {
		for _, g := range Corpus {
			fmt.Printf("%-14s module=%-10s accept=%-22s %s\n", g.Name, g.Module, g.Accept, firstLine(g.Prompt))
		}
		return nil
	}

	selected := selectGoals(*goals)
	if len(selected) == 0 {
		return fmt.Errorf("no goals selected (corpus: %s); see -list", corpusNames())
	}
	if *repeat < 1 {
		return fmt.Errorf("-repeat must be >= 1")
	}
	if *maxSteps < 1 || *maxRetries < 1 {
		return fmt.Errorf("-max-steps and -max-retries must be >= 1")
	}
	loopName, err := normalizeLoop(*loop)
	if err != nil {
		return err
	}

	rootDir := *root
	if rootDir == "" {
		rootDir = filepath.Join("bench-runs", time.Now().Format("20060102-150405"))
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return fmt.Errorf("create bench root: %w", err)
	}
	outPath := *out
	if outPath == "" {
		outPath = filepath.Join(rootDir, "report.json")
	}

	// Ctrl+C stops the bench between goals (and interrupts the running one),
	// keeping whatever results already landed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bc := benchConfig{
		BaseURL:    *baseURL,
		Model:      *model,
		APIKey:     *apiKey,
		ExtraJSON:  *extra,
		MaxSteps:   *maxSteps,
		MaxRetries: *maxRetries,
		Parallel:   *parallel,
		Isolate:    *isolate,
		GitCommit:  *gitCommit,
		Timeout:    *timeout,
		Root:       rootDir,
		Loop:       loopName,
		Pricing:    cost.DefaultPricing(),
	}

	meta := reportMeta{
		Model:   *model,
		BaseURL: *baseURL,
		Extra:   *extra,
		Loop:    loopName,
		Started: time.Now(),
	}
	fmt.Printf("divvy-bench: model=%s endpoint=%s loop=%s goals=%d repeat=%d root=%s\n",
		*model, *baseURL, loopName, len(selected), *repeat, rootDir)

	var results []RunResult
	for rep := 1; rep <= *repeat; rep++ {
		for _, g := range selected {
			if ctx.Err() != nil {
				break
			}
			fmt.Printf("\n=== %s (repeat %d/%d)\n", g.Name, rep, *repeat)
			res := runGoal(ctx, g, rep, bc)
			fmt.Printf("--- %s: ok=%v wall=%s calls=%d tokens=%d leaves=%d/%d\n",
				res.Goal, res.OK, res.Wall(), res.LLMCalls, res.TotalTokens, res.LeavesDone, res.Leaves)
			if res.Loop == loopDirect {
				switch {
				case !res.AcceptRan:
					fmt.Printf("    acceptance: not run (%s)\n", oneLine(res.AcceptOut))
				case res.AcceptOK:
					fmt.Printf("    acceptance: passed\n")
				default:
					fmt.Printf("    acceptance: FAILED %s\n", res.AcceptOut)
				}
			}
			if res.Error != "" {
				fmt.Printf("    error: %s\n", res.Error)
			}
			fmt.Printf("    workspace: %s\n", res.Workdir)
			results = append(results, res)
		}
	}

	agg := aggregate(results)
	meta.Finished = time.Now()
	fmt.Print("\n" + renderTable(results))
	fmt.Print(renderAggregate(agg))

	if err := writeReport(outPath, benchReport{Meta: meta, Results: results, Aggregate: agg}); err != nil {
		fmt.Fprintf(os.Stderr, "divvy-bench: write report: %v\n", err)
	} else {
		fmt.Printf("report: %s\n", outPath)
	}
	return nil
}

func corpusNames() string {
	names := ""
	for i, g := range Corpus {
		if i > 0 {
			names += ", "
		}
		names += g.Name
	}
	return names
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
