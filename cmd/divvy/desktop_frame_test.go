package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestDesktopFramePersistence covers the window-frame file: missing file,
// round trip, corrupt file and implausible frames are all refused so a bad
// file can never strand the window off-screen.
func TestDesktopFramePersistence(t *testing.T) {
	s := &desktopShell{framePath: filepath.Join(t.TempDir(), "desktop-window.json")}

	if _, ok := s.loadFrame(); ok {
		t.Fatal("missing frame file must not load")
	}

	want := desktopFrame{X: 111, Y: 222, W: 800, H: 600}
	if err := s.saveFrame(want); err != nil {
		t.Fatal(err)
	}
	got, ok := s.loadFrame()
	if !ok || got != want {
		t.Fatalf("round trip mismatch: %+v ok=%v", got, ok)
	}

	// Implausible frames are refused on save: the file keeps its last good
	// content instead of stranding the window.
	if err := s.saveFrame(desktopFrame{X: -9999, Y: -9999, W: 50, H: 20}); err == nil {
		t.Fatal("implausible frame must be refused")
	}
	raw, err := os.ReadFile(s.framePath)
	if err != nil {
		t.Fatal(err)
	}
	var kept desktopFrame
	if json.Unmarshal(raw, &kept) != nil || kept != want {
		t.Fatalf("refused save clobbered the file: %s", raw)
	}

	if err := os.WriteFile(s.framePath, []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.loadFrame(); ok {
		t.Fatal("corrupt frame file must not load")
	}
}
