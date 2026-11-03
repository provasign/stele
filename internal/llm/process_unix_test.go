//go:build unix

package llm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCancellationStopsChildHoldingPipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := run(ctx, "sh", []string{"-c", "sleep 30 & wait"}, "")
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("cancellation left the worker/pipe running: %v, %s", err, time.Since(start))
	}
}

// A signal that stops Stele also stops the model CLI it is running: the CLI
// is in a process group of its own, which the signal does not reach.
func TestSignalStopsRunningCLI(t *testing.T) {
	if os.Getenv("STELE_SIGNAL_HELPER") != "" {
		// The helper: run a CLI that leaves a long sleep behind, and wait.
		_, _, _ = run(context.Background(), "sh", []string{"-c", "sleep 60 & echo $! > " + os.Getenv("STELE_SIGNAL_HELPER") + "; wait"}, "")
		return
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	helper := exec.Command(os.Args[0], "-test.run", "^TestSignalStopsRunningCLI$")
	helper.Env = append(os.Environ(), "STELE_SIGNAL_HELPER="+pidFile)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	var child int
	for i := 0; i < 100 && child == 0; i++ {
		if b, err := os.ReadFile(pidFile); err == nil {
			child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if child == 0 {
		helper.Process.Kill()
		t.Fatal("the helper's CLI did not start")
	}
	_ = helper.Process.Signal(syscall.SIGTERM)
	_ = helper.Wait()
	for i := 0; i < 40; i++ {
		if syscall.Kill(child, 0) != nil {
			return // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	syscall.Kill(child, syscall.SIGKILL)
	t.Fatalf("the CLI's child %d outlived Stele's SIGTERM", child)
}
