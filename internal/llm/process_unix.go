//go:build unix

package llm

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync"
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

// live are the process groups of the model CLIs running now. Each runs in
// its own group, so a signal that stops Stele (SIGTERM from the agent that
// ran it, ^C) did not reach them: a stopped check left a Codex CLI running
// under PID 1 (2026-11-02). On such a signal they are killed first.
var live = struct {
	sync.Mutex
	groups map[int]bool
	once   sync.Once
}{groups: map[int]bool{}}

// track records a started CLI's process group until the returned function
// is called.
func track(cmd *exec.Cmd) func() {
	live.once.Do(stopGroupsOnSignal)
	pid := cmd.Process.Pid
	live.Lock()
	live.groups[pid] = true
	live.Unlock()
	return func() {
		live.Lock()
		delete(live.groups, pid)
		live.Unlock()
	}
}

func stopGroupsOnSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		sig := <-ch
		live.Lock()
		for pid := range live.groups {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		live.Unlock()
		// Then stop as the signal would have.
		signal.Reset(sig)
		_ = syscall.Kill(os.Getpid(), sig.(syscall.Signal))
	}()
}
