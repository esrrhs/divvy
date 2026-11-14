package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The workspace status is a safety net: it tells the model there may be edits
// it must not overwrite. When it cannot be read, saying nothing reads as "the
// workspace is clean" — the opposite of the truth.
func TestDirectWorkspaceStatus_SaysSoWhenGitFails(t *testing.T) {
	orig := directStatusProbe
	defer func() { directStatusProbe = orig }()
	directStatusProbe = func(ctx context.Context, root string) ([]byte, error) {
		return nil, errors.New("git timed out")
	}

	// A real repository, so the probe is reached.
	dir := t.TempDir()
	initGitRepo(t, dir)
	out := directWorkspaceStatus(dir)
	if out == "" {
		t.Fatal("an unreadable status must not be reported as an empty workspace")
	}
	if !strings.Contains(out, "COULD NOT BE READ") {
		t.Fatalf("the model must be told the status is unknown:\n%s", out)
	}
}
