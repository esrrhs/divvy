package engine

import (
	"strings"
	"testing"

	"github.com/esrrhs/go_llm_engine/pkg/models"
)

func TestRenderVisualTreeShowsTokens(t *testing.T) {
	tree := NewTaskTree("sess", "goal", "goal")
	if _, err := tree.AddChild(tree.RootID, "alpha", "Alpha", "do alpha", models.NodeTypeLeaf); err != nil {
		t.Fatal(err)
	}
	if err := tree.UpdateNode("alpha", func(n *models.TaskNode) error {
		n.TokenUsage = models.TokenUsage{
			Calls:            2,
			PromptTokens:     700,
			CompletionTokens: 300,
			TotalTokens:      1000,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	out := tree.RenderVisualTree()
	if !strings.Contains(out, "[1.0k tok]") {
		t.Fatalf("token usage missing from render:\n%s", out)
	}

	// Root has no usage: no token marker on its line.
	if !strings.Contains(out, "(Root: root)\n") {
		t.Fatalf("unexpected root line:\n%s", out)
	}
}

func TestFormatTokens(t *testing.T) {
	cases := map[int]string{
		0:         "0",
		999:       "999",
		1000:      "1.0k",
		12345:     "12.3k",
		1_500_000: "1.5M",
	}
	for in, want := range cases {
		if got := formatTokens(in); got != want {
			t.Fatalf("formatTokens(%d) = %q, want %q", in, got, want)
		}
	}
}
