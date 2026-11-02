//go:build !unix

package llm

import (
	"os/exec"
	"time"
)

func containProcess(cmd *exec.Cmd) {
	cmd.WaitDelay = time.Second
}
