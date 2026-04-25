//go:build !unix

package agent

import (
	"errors"
	"os/exec"
)

// setProcessGroup is a no-op on non-Unix platforms.  hearsay doesn't
// ship Windows builds today, but this stub keeps the package
// compilable on `go vet`-only Windows lints.
func setProcessGroup(cmd *exec.Cmd) {}

func getProcessGroup(cmd *exec.Cmd) (int, error) {
	return 0, errors.New("process groups not supported on this platform")
}
