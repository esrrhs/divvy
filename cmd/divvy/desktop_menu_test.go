package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/esrrhs/divvy/pkg/agent"
)

// TestDesktopMenuNewConversationUsesShownProject: the shell remembers the
// workdir of the conversation the window is showing (from the detail
// responses it already watches for the title), and the menu's 新建对话
// creates the next conversation in THAT project.
func TestDesktopMenuNewConversationUsesShownProject(t *testing.T) {
	var gotWorkdir string
	done := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/conversations" && r.Method == http.MethodPost {
			var v struct {
				WorkDir string `json:"workdir"`
			}
			_ = json.NewDecoder(r.Body).Decode(&v)
			gotWorkdir = v.WorkDir
			close(done)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"conv_new"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer backend.Close()

	shell := &desktopShell{
		backendURL:  backend.URL,
		backendPort: "0",
		appCtx:      context.Background(),
		lastWorkdir: "/tmp/divvy-smoke", // the project the window is showing
	}
	shell.newConversationFromMenu(agent.Config{WorkDir: "/default/workdir", Model: "m", BaseURL: "http://x"})

	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("menu create never fired")
	}
	if gotWorkdir != "/tmp/divvy-smoke" {
		t.Fatalf("menu used the launch workdir instead of the shown project: %q", gotWorkdir)
	}

	// No conversation watched yet: falls back to the launch workdir.
	shell.lastWorkdir = ""
	shell.newConversationFromMenu(agent.Config{WorkDir: "/default/workdir"})
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("menu create never fired (fallback)")
	}
	if gotWorkdir != "/default/workdir" {
		t.Fatalf("fallback workdir wrong: %q", gotWorkdir)
	}
}
