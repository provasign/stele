package llm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestHedge: a call that stalls is sent again and the first answer wins; a
// call that fails before the hedge time fails at once.
func TestHedge(t *testing.T) {
	var calls atomic.Int32
	stallsFirst := func(ctx context.Context) (*Reply, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done() // stalls until the answer of the second cancels it
			return nil, ctx.Err()
		}
		return &Reply{JSON: "{}"}, nil
	}
	r, err := hedge(context.Background(), 10*time.Millisecond, stallsFirst)
	if err != nil || r == nil || calls.Load() != 2 {
		t.Fatalf("stalled call: %v %v after %d calls", r, err, calls.Load())
	}

	calls.Store(0)
	fails := func(ctx context.Context) (*Reply, error) {
		calls.Add(1)
		return nil, errors.New("crashed")
	}
	if _, err := hedge(context.Background(), time.Hour, fails); err == nil || calls.Load() != 1 {
		t.Fatalf("failed call: %v after %d calls", err, calls.Load())
	}

	calls.Store(0)
	fast := func(ctx context.Context) (*Reply, error) {
		calls.Add(1)
		return &Reply{JSON: "{}"}, nil
	}
	if _, err := hedge(context.Background(), time.Hour, fast); err != nil || calls.Load() != 1 {
		t.Fatalf("fast call: %v after %d calls", err, calls.Load())
	}
}
