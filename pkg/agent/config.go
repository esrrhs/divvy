package agent

import (
	"os"
	"strings"
	"time"
)

// Config controls the engine run.
type Config struct {
	WorkDir   string
	DataDir   string
	SessionID string
	Goal      string

	APIKey    string
	BaseURL   string
	Model     string
	ExtraJSON string

	Temperature float64
	MaxTokens   int

	RequestTimeout     time.Duration
	MinRequestInterval time.Duration

	MaxSteps       int
	MaxRetries     int // 0 = retry forever
	MaxDepth       int
	MaxSubtasks    int
	MaxRedecompose int
	DecomposeTries int  // 0 = retry forever
	Parallel       int  // concurrent leaf executions; <= 1 means serial
	Isolate        bool // run each leaf in a mirror copy of the workspace; merge only on success

	RetryMinInterval time.Duration
	RetryMaxInterval time.Duration

	NativeTools bool
	Stream      bool
	Verbose     bool

	// WebEnabled enables outbound web_search/web_fetch for leaves;
	// SearchURL is the query template (empty = built-in DuckDuckGo lite;
	// a SearXNG instance with format=json is supported).
	WebEnabled bool
	SearchURL  string

	// BrowserEnabled enables headless Chrome tools for leaves.
	BrowserEnabled bool

	GitCommit bool // commit each leaf's merged changes to the workdir git repo
	Strict    bool // plan mode: fail on plan-check warnings

	// Cost & budget. PricingJSON is a custom price table as JSON text or a
	// path to a JSON file; empty means the built-in table only. MaxCost and
	// BudgetTokens are session-wide ceilings (0 = unlimited) that include
	// tokens spent before a resume.
	PricingJSON  string
	MaxCost      float64 // USD
	BudgetTokens int
}

// DefaultConfig fills in usable defaults.
func DefaultConfig() Config {
	return Config{
		WorkDir:          ".",
		DataDir:          ".divvy",
		BaseURL:          firstEnv("OPENAI_BASE_URL", "LLM_BASE_URL", "https://api.openai.com/v1"),
		APIKey:           firstEnv("OPENAI_API_KEY", "LLM_API_KEY", ""),
		Model:            firstEnv("OPENAI_MODEL", "LLM_MODEL", "gpt-4o-mini"),
		Temperature:      0.2,
		MaxTokens:        4096,
		RequestTimeout:   120 * time.Second,
		MaxSteps:         20,
		MaxRetries:       0,
		MaxDepth:         4,
		MaxSubtasks:      6,
		MaxRedecompose:   2,
		DecomposeTries:   0,
		Parallel:         1,
		RetryMinInterval: time.Second,
		RetryMaxInterval: 30 * time.Second,
		Stream:           true,
		PricingJSON:      strings.TrimSpace(os.Getenv("LLM_PRICING")),
	}
}

// RequiresAPIKey is false for typical local OpenAI-compatible servers.
func (c Config) RequiresAPIKey() bool {
	u := strings.ToLower(c.BaseURL)
	return !strings.Contains(u, "localhost") && !strings.Contains(u, "127.0.0.1") && !strings.Contains(u, "0.0.0.0")
}

func firstEnv(keys ...string) string {
	for i, k := range keys {
		if i == len(keys)-1 && !isEnvKey(k) {
			return k
		}
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func isEnvKey(s string) bool {
	return strings.Contains(s, "_") && strings.ToUpper(s) == s
}
