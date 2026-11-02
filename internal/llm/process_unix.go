//go:build unix

package llm

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// A CLI can spawn worker processes which keep stdout/stderr open after its
// parent dies. Cancel the whole group and bound pipe draining on timeout.
func containProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
}
