//go:build !darwin

// Desktop-shell stub for platforms without the Wails macOS shell in this
// build. The web UI remains fully available through -serve.
package main

import (
	"fmt"

	"github.com/esrrhs/divvy/pkg/agent"
)

func desktopSupported() bool { return false }

func runDesktop(cfg agent.Config, log *agent.Logger) error {
	return fmt.Errorf("desktop mode is not available in this build (macOS only); use -serve for the web UI")
}
