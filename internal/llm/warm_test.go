package llm

import (
	"context"
	"strings"
	"testing"
)

type countingClient struct {
	caches bool
	calls  int
}

func (c *countingClient) Complete(context.Context, Request) (*Reply, error) {
	c.calls++
	return &Reply{JSON: "{}", Usage: Usage{OutputTokens: 5}}, nil
}
func (c *countingClient) Name() string        { return "counting" }
func (c *countingClient) CachesPrompts() bool { return c.caches }

// A warm-up goes out only where it saves: two or more requests to follow,
// a backend that caches, and a system prompt worth caching.
func TestWarmOnlyWhereTheCacheSaves(t *testing.T) {
	long, short := strings.Repeat("x", warmMin), "short"
	for _, tc := range []struct {
		caches bool
		n      int
		system string
		want   int
	}{
		{true, 3, long, 1},
		{true, 1, long, 0},
		{false, 3, long, 0},
		{true, 3, short, 0},
	} {
		c := &countingClient{caches: tc.caches}
		r := Warm(context.Background(), c, tc.n, Request{System: tc.system})
		if c.calls != tc.want || (tc.want == 1) != (r != nil) {
			t.Errorf("caches %v, n %d, system %d bytes: %d calls, reply %v", tc.caches, tc.n, len(tc.system), c.calls, r)
		}
	}
	if !CachesPrompts(&claudeCLI{}) || !CachesPrompts(&codexCLI{}) || CachesPrompts(&commandCLI{}) {
		t.Error("the CLIs cache; a custom command is not known to")
	}
}
