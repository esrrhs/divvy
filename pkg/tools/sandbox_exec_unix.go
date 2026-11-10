//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// configureLeafCommand puts the shell in its own process group so a
// timeout can signal the whole group, including background children.
func configureLeafCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killLeafProcessTree force-kills every process in the shell's group.
// A negative pid targets the process group led by the shell. ESRCH
// (empty group / already reaped) is expected and ignored.
func killLeafProcessTree(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
