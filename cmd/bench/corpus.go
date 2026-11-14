// Command divvy-bench is the weak-model benchmark harness for divvy.
//
// The engine's thesis is that engineering constraints, not model cleverness,
// carry complex goals on small models — but "it works on weak models" used to
// be backed only by scripted mock end-to-end tests. This harness makes the
// claim measurable: a fixed corpus of small, self-verifying goals is executed
// against a real OpenAI-compatible endpoint (a local Ollama by default), and
// each run reports success, wall time, token usage, cost, and the engine's
// own failure counters (verify failures, retries, stalls, re-splits).
//
// The corpus is intentionally small and versioned in code: a benchmark whose
// goals drift between runs cannot show regressions. Most goals are Go (offline
// go build/go test); a small CMake/C++ entry exercises ctest normalize, build/
// scrub, and VERIFY TIMED OUT fingerprint paths.
package main

import "strings"

// Goal is one benchmark task. Seed files land in a fresh workspace before the
// run; the prompt is the goal divvy receives; Module is the go module name
// seeded via `go mod init` (empty means the goal runs in a generic workspace).
//
// Accept is the command that decides the goal objectively. Tree mode gets its
// acceptance from the plan's DoD, so Accept is unused there; direct mode has no
// task contract, so the harness runs this itself once the loop returns. That is
// the only fair way to compare the two loops: same goal, same command, same
// workspace state.
type Goal struct {
	Name   string
	Module string
	Prompt string
	Seed   map[string]string
	Accept string

	// Guard lists seed files the goal forbids changing. They are usually the
	// very test that defines the task: without this, a model can "pass" by
	// editing the assertion instead of satisfying it, and the bench would
	// report a capability that does not exist.
	Guard []string

	// LeafSteps pins the per-leaf tool budget for this goal (0 uses
	// -max-steps). A goal that is too big for one leaf context is the only
	// way to test the engine's actual claim — that decomposition carries
	// work a single context cannot — so this goal shrinks the leaf budget
	// instead of growing the task until it happens to overflow on its own.
	// Direct mode is unaffected: its budget is -direct-steps by definition.
	LeafSteps int
}

// Corpus is the fixed goal set. Keep it small: a full bench pass costs real
// model time, and a handful of distinct task shapes (create-from-scratch,
// fix-a-bug, build-a-service) already exposes the engine's weak points:
// decomposition granularity, diagnosis of failing tests, and end-to-end
// acceptance.
var Corpus = []Goal{
	{
		Name:   "string-utils",
		Module: "strutil",
		Prompt: "用 Go 新建包 strutil 并实现三个函数：" +
			"Reverse(s string) string 反转字符串（按 rune，正确处理中文）；" +
			"IsPalindrome(s string) bool 判断回文（忽略大小写，只比较字母和数字）；" +
			"WordCount(s string) int 统计英文单词数。" +
			"为每个函数写表驱动单元测试，覆盖空字符串、大小写混合、含标点的输入。" +
			"只用标准库。",
		Accept: "go build ./... && go test ./...",
	},
	{
		Name:   "fix-average",
		Module: "calc",
		Prompt: "workspace 里的 calc 包有 bug：Average 的除数是差一错误，且对空切片会得到 NaN。" +
			"修复它，使 Average 对空切片返回 0、对非空切片返回正确平均值，" +
			"并让 go test ./... 全部通过。不要改动测试文件。",
		Accept: "go test ./...",
		Seed: map[string]string{
			"calc.go": `package calc

// Average returns the arithmetic mean of xs.
func Average(xs []int) float64 {
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return float64(sum) / float64(len(xs)+1)
}
`,
			"calc_test.go": `package calc

import "testing"

func TestAverage(t *testing.T) {
	cases := []struct {
		in   []int
		want float64
	}{
		{nil, 0},
		{[]int{}, 0},
		{[]int{1, 2, 3}, 2},
		{[]int{5}, 5},
		{[]int{-2, 2}, 0},
	}
	for _, c := range cases {
		if got := Average(c.in); got != c.want {
			t.Errorf("Average(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
`,
		},
	},
	{
		Name:   "http-health",
		Module: "healthsrv",
		Prompt: "用 Go 写一个 HTTP 服务：监听 :18080，GET /health 返回 200 和字符串 ok，" +
			"其它方法返回 405；并写单元测试。只用标准库。",
		Accept: "go build ./... && go test ./...",
	},
	{
		// The only goal that cannot be done in one leaf without ignoring the
		// prompt: three files with three separate responsibilities. The
		// contract lives in a seeded test that the model may not touch, so a
		// pass means the API was implemented, not that the test was relaxed.
		Name:   "config-cli",
		Module: "cfgcli",
		Prompt: "workspace 里只有 main_test.go：它定义了契约，但还没有任何实现。" +
			"读它，然后按它实现：config.go 放 Config 结构体与 LoadConfig(path) (Config, error)（读 JSON，" +
			"未出现的字段取零值，未知字段忽略）；run.go 放 Run(args []string, out io.Writer) error" +
			"（从 args[0] 读配置，向 out 打印形如 \"name=<name> retries=<n>\" 的一行）；" +
			"main.go 只留入口，把 os.Args[1:] 和 os.Stdout 交给 Run。" +
			"让 go test ./... 通过。不要修改 main_test.go。",
		Accept:    "go build ./... && go test ./...",
		Guard:     []string{"main_test.go"},
		LeafSteps: 6,
		Seed: map[string]string{
			"main_test.go": `package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig(t *testing.T) {
	p := writeConfig(t, ` + "`" + `{"name":"prod","retries":3,"verbose":true}` + "`" + `)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Name != "prod" || cfg.Retries != 3 || !cfg.Verbose {
		t.Fatalf("LoadConfig parsed %+v", cfg)
	}
}

func TestRunPrintsSummary(t *testing.T) {
	p := writeConfig(t, ` + "`" + `{"name":"dev","retries":1}` + "`" + `)
	var out bytes.Buffer
	if err := Run([]string{p}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "name=dev") || !strings.Contains(got, "retries=1") {
		t.Fatalf("Run printed %q", got)
	}
}
`,
		},
	},
	{
		Name: "cmake-add",
		Prompt: "用 CMake/C++ 实现 add(int,int)，写 CMakeLists.txt、src/add.cpp、src/add.h、tests/test_add.cpp；" +
			"用 ctest 跑通（ctest --test-dir build --output-on-failure -R add）。" +
			"不要依赖网络；构建目录必须是 build/。",
		Accept: "ctest --test-dir build --output-on-failure -R add",
		Seed: map[string]string{
			"CMakeLists.txt": "cmake_minimum_required(VERSION 3.14)\n" +
				"project(addlib LANGUAGES CXX)\n" +
				"set(CMAKE_CXX_STANDARD 17)\n" +
				"enable_testing()\n" +
				"add_library(add STATIC src/add.cpp)\n" +
				"target_include_directories(add PUBLIC src)\n" +
				"add_executable(test_add tests/test_add.cpp)\n" +
				"target_link_libraries(test_add PRIVATE add)\n" +
				"add_test(NAME add COMMAND test_add)\n",
			"src/add.h":          "#pragma once\nint add(int a, int b);\n",
			"src/add.cpp":        "#include \"add.h\"\nint add(int a, int b) { return a + b + 1; } // intentional off-by-one for the model to fix\n",
			"tests/test_add.cpp": "#include \"add.h\"\nint main() {\n  if (add(2, 3) != 5) return 1;\n  if (add(0, 0) != 0) return 2;\n  return 0;\n}\n",
		},
	},
}

// goalByName resolves a corpus entry for tests and -goals filtering.
func goalByName(name string) (Goal, bool) {
	for _, g := range Corpus {
		if g.Name == name {
			return g, true
		}
	}
	return Goal{}, false
}

// selectGoals filters the corpus by a comma-separated name list; an empty
// filter selects everything, preserving corpus order.
func selectGoals(filter string) []Goal {
	if strings.TrimSpace(filter) == "" {
		out := make([]Goal, len(Corpus))
		copy(out, Corpus)
		return out
	}
	want := map[string]bool{}
	for _, name := range strings.Split(filter, ",") {
		if n := strings.TrimSpace(name); n != "" {
			want[n] = true
		}
	}
	var out []Goal
	for _, g := range Corpus {
		if want[g.Name] {
			out = append(out, g)
		}
	}
	return out
}
