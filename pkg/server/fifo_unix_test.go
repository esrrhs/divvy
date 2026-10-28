//go:build !windows

package server

import (
	"syscall"
	"testing"
)

func makeTestFifo(t *testing.T, path string) bool {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
	return true
}
