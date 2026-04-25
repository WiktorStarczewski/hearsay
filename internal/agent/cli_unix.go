//go:build unix

package agent

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup runs the subprocess in its own process group so a
// kill signal can be sent to the whole group on context cancel.
// Important on Linux (`/bin/sh` is `dash`, which does not forward
// SIGTERM to its child) and harmless on macOS / *BSD.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// getProcessGroup returns the subprocess's process-group ID, which is
// equal to its PID after Setpgid.  Returns an error if the process
// hasn't started yet (Cmd.Process == nil).
func getProcessGroup(cmd *exec.Cmd) (int, error) {
	if cmd.Process == nil {
		return 0, errors.New("process not started")
	}
	return syscall.Getpgid(cmd.Process.Pid)
}
