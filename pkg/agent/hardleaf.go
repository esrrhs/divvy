package agent

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/esrrhs/divvy/pkg/models"
)

// DefaultHardMaxSteps is the tool-call budget for "hard" leaves (integrate /
// server / cmake+ctest style) when HardMaxSteps is unset. Easy leaves keep
// MaxSteps (default 20); hard leaves need more room for configure+build+smoke.
const DefaultHardMaxSteps = 40

// DefaultHardRequestTimeout is the per-LLM-request timeout used while a hard
// leaf is executing when HardRequestTimeout is unset.
const DefaultHardRequestTimeout = 180 * time.Second

// isHardLeaf reports whether a leaf needs the elevated MaxSteps / request
// timeout. Heuristics cover integrate/server/smoke titles, CMake/CTest DoD,
// and end-to-end shell that starts a process then curls it.
func isHardLeaf(node *models.TaskNode) bool {
	if node == nil {
		return false
	}
	blob := strings.ToLower(strings.Join([]string{
		node.ID, node.Title, node.Description,
		strings.Join(node.DoD.Commands, "\n"),
	}, "\n"))
	for _, k := range []string{
		"integrate", "integration", "e2e", "end-to-end", "end_to_end",
		"smoke", "server", "http", "listen", "daemon", "验收", "联调",
	} {
		if strings.Contains(blob, k) {
			return true
		}
	}
	for _, c := range node.DoD.Commands {
		lc := strings.ToLower(c)
		if strings.Contains(lc, "ctest") || strings.Contains(lc, "cmake") {
			return true
		}
		if strings.Contains(lc, "curl") && (strings.Contains(c, "&") || strings.Contains(lc, "sleep")) {
			return true
		}
		if strings.Contains(lc, "go build") && strings.Contains(lc, "-o") {
			return true
		}
	}
	return false
}

// leafMaxSteps returns the tool-call budget for one leaf attempt. Hard leaves
// get HardMaxSteps (or DefaultHardMaxSteps / DIVVY_HARD_MAX_STEPS) when that
// value is larger than the base MaxSteps; easy leaves keep MaxSteps.
func (o *Orchestrator) leafMaxSteps(node *models.TaskNode) int {
	base := o.cfg.MaxSteps
	if base < 1 {
		base = 12
	}
	if !isHardLeaf(node) {
		return base
	}
	hard := o.cfg.HardMaxSteps
	if hard <= 0 {
		hard = envInt("DIVVY_HARD_MAX_STEPS", DefaultHardMaxSteps)
	}
	if hard < base {
		return base
	}
	return hard
}

// leafRequestTimeout returns the per-request timeout to apply while chatting
// for a leaf. Hard leaves may use HardRequestTimeout / DIVVY_HARD_TIMEOUT.
func (o *Orchestrator) leafRequestTimeout(node *models.TaskNode) time.Duration {
	base := o.cfg.RequestTimeout
	if base <= 0 {
		base = 120 * time.Second
	}
	if !isHardLeaf(node) {
		return base
	}
	hard := o.cfg.HardRequestTimeout
	if hard <= 0 {
		if v := strings.TrimSpace(os.Getenv("DIVVY_HARD_TIMEOUT")); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				hard = d
			}
		}
	}
	if hard <= 0 {
		hard = DefaultHardRequestTimeout
	}
	if hard < base {
		return base
	}
	return hard
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
