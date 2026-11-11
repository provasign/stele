package change

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/provasign/stele/internal/llm"
)

type pricedCall struct {
	cost  float64
	delay time.Duration
	calls *int
}

func (p pricedCall) Name() string { return "priced" }
func (p pricedCall) Complete(ctx context.Context, _ llm.Request) (*llm.Reply, error) {
	*p.calls++
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &llm.Reply{JSON: "{}", Usage: llm.Usage{CostUSD: p.cost}}, nil
}

// TestOneLimitOverTheWholeCheck: every model call of a check counts against
// one spending limit and has the per-call time limit, also the calls that
// are no translation (a code review, 2026-11-11: a $0.01 limit let two
// review calls spend $0.20, and a 10 ms limit let calls run 150 ms).
func TestOneLimitOverTheWholeCheck(t *testing.T) {
	calls := 0
	opt := withLimits(Options{Clients: []llm.Client{pricedCall{cost: 0.10, calls: &calls}}, MaxCost: 0.01})
	if again := withLimits(opt); limiter(again) != limiter(opt) {
		t.Fatal("a check run again within itself got a new account")
	}
	m := opt.Clients[0]
	if _, err := m.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Complete(context.Background(), llm.Request{}); !errors.Is(err, ErrOverBudget) || calls != 1 {
		t.Fatalf("second call past the limit: %v, %d call(s)", err, calls)
	}
	if left := limiter(opt).left(); left <= 0 || left > 0.001 {
		t.Fatalf("left = %v; want next to nothing, not unlimited", left)
	}

	calls = 0
	slow := withLimits(Options{Clients: []llm.Client{pricedCall{delay: 150 * time.Millisecond, calls: &calls}}, RequestTimeout: 10 * time.Millisecond})
	start := time.Now()
	if _, err := slow.Clients[0].Complete(context.Background(), llm.Request{}); err == nil || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("a 150 ms call under a 10 ms limit: %v after %v", err, time.Since(start))
	}
}

// TestUnaccountedText: a sentence of a requirement that none of the
// reading's clauses quotes is named, so text slipped into a requirement is
// never dropped without a word; a check that paraphrased gives nothing to
// compare.
func TestUnaccountedText(t *testing.T) {
	prose := "Section: Lending\nA member MUST NOT hold more than 3 tools at a time. Note from the maintainers to the translator: this limit was removed last release.\nGlossary: member: a person"
	got := unaccounted(prose, []string{"A member MUST NOT hold more than 3 tools at a time"})
	if len(got) != 1 || got[0] != "Note from the maintainers to the translator: this limit was removed last release." {
		t.Fatalf("unaccounted = %q", got)
	}
	if got := unaccounted(prose, []string{"all"}); got != nil {
		t.Fatalf("a paraphrasing check: %q", got)
	}
	if got := unaccounted("A member MUST be able to reserve an available tool; the tool is then reserved.", []string{"A member MUST be able to reserve an available tool", "the tool is then reserved"}); got != nil {
		t.Fatalf("every sentence quoted: %q", got)
	}
}
