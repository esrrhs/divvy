//go:build windows

package server

import "testing"

func makeTestFifo(t *testing.T, path string) bool {
	t.Helper()
	return false
}
