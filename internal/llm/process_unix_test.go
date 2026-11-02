//go:build unix

package llm

import (
	"context"
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
