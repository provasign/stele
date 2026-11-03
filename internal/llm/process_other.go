//go:build !unix

package llm

import (
	"os/exec"
	"time"
)

func containProcess(cmd *exec.Cmd) {
	cmd.WaitDelay = time.Second
}

// track has nothing to do here: the CLI is not in a group of its own.
func track(*exec.Cmd) func() { return func() {} }
