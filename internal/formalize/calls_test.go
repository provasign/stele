package formalize

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
)

type contextCompletion func(context.Context, llm.Request) (*llm.Reply, error)

func (f contextCompletion) Name() string { return "fixture" }
func (f contextCompletion) Complete(ctx context.Context, req llm.Request) (*llm.Reply, error) {
	return f(ctx, req)
}

func TestJournalCountsFailuresAndInterruptedCallsAcrossResume(t *testing.T) {
	dir := t.TempDir()
	// Simulate a process killed after recording an attempt but before its reply.
	e, _ := json.Marshal(callEvent{ID: 1, Event: "started", At: time.Now()})
	if err := os.WriteFile(filepath.Join(dir, "requests.jsonl"), append(e, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	m := contextCompletion(func(ctx context.Context, _ llm.Request) (*llm.Reply, error) { <-ctx.Done(); return nil, ctx.Err() })
	j, err := newCallJournal(m, dir, Options{MaxCalls: 2, RequestTimeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Complete(context.Background(), llm.Request{System: "abc", Schema: "{}"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout not applied: %v", err)
	}
	j.close()
	j, err = newCallJournal(m, dir, Options{MaxCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	if got := j.stats(); got != (CallStats{Started: 2, Failed: 1, Interrupted: 1}) {
		t.Fatalf("lost calls after resume: %+v", got)
	}
	if _, err := j.Complete(context.Background(), llm.Request{}); err == nil {
		t.Fatal("resume bypassed total budget")
	}
}

func TestJournalConcurrentBudget(t *testing.T) {
	var calls atomic.Int32
	m := completionFunc(func(llm.Request) (*llm.Reply, error) { calls.Add(1); return &llm.Reply{JSON: "{}"}, nil })
	j, err := newCallJournal(m, t.TempDir(), Options{MaxCalls: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { _, _ = j.Complete(context.Background(), llm.Request{}) })
	}
	wg.Wait()
	if calls.Load() != 3 || j.stats() != (CallStats{Started: 3, Completed: 3}) {
		t.Fatalf("parallel calls bypassed budget: %d, %+v", calls.Load(), j.stats())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := calls.Load()
	if _, err := j.Complete(ctx, llm.Request{}); err == nil || calls.Load() != before {
		t.Fatalf("cancelled call was sent: %v", err)
	}
}

func TestJournalPreservesRawReplyForOfflineReplay(t *testing.T) {
	dir := t.TempDir()
	want := &llm.Reply{JSON: "{malformed", Model: "fixture", Usage: llm.Usage{InputTokens: 7}}
	m := completionFunc(func(llm.Request) (*llm.Reply, error) { return want, nil })
	j, err := newCallJournal(m, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	req := llm.Request{System: "shared symbols", Turns: []llm.Turn{{Role: "user", Text: "statement_id: s1"}}, Schema: "{}"}
	if _, err := j.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests", "000001.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Request llm.Request
		Reply   *llm.Reply
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Reply == nil || got.Reply.JSON != want.JSON || got.Request.System != req.System || got.Request.Turns[0].Text != req.Turns[0].Text {
		t.Fatalf("raw evidence lost: %+v", got)
	}
}

// A cost budget stops new calls once this run's calls have reported it.
func TestCostBudget(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	stmts[1].Source = "b.md#L1" // two requests
	opt.Batch, opt.Parallel, opt.MaxCost = 1, 1, 0.5
	calls := 0
	m := completionFunc(func(llm.Request) (*llm.Reply, error) {
		calls++
		r := compactFixtureReply(true, compactRule("s1", "order.paid = true"))
		r.Usage.CostUSD = 0.6
		return r, nil
	})
	_, err := Run(context.Background(), m, stmts, dir, opt)
	if err == nil || !strings.Contains(err.Error(), "cost budget reached") || calls != 1 {
		t.Fatalf("calls %d, err %v; want one call, then the cost budget", calls, err)
	}
	// The statement translated before the budget ran out is in a registry
	// that loads.
	if src, err := os.ReadFile(filepath.Join(dir, "rules.yaml")); err != nil || !strings.Contains(string(src), "order.paid") {
		t.Errorf("rules.yaml after the budget: %v\n%s", err, src)
	}
	if _, err := ir.LoadDir(dir); err != nil {
		t.Errorf("the registry after the budget does not load: %v", err)
	}
}

// Parallel calls stay within the cost budget: once a call has reported its
// cost, later calls start only while the spend, the calls in flight and their
// own expected cost fit, instead of all starting before any reports.
func TestCostBudgetHoldsUnderParallelCalls(t *testing.T) {
	var mu sync.Mutex
	started := 0
	m := completionFunc(func(llm.Request) (*llm.Reply, error) {
		mu.Lock()
		started++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		return &llm.Reply{JSON: "{}", Usage: llm.Usage{CostUSD: 0.1}}, nil
	})
	j, err := newCallJournal(m, t.TempDir(), Options{MaxCost: 0.35})
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j.Complete(context.Background(), llm.Request{System: "the same prompt for every call"})
		}()
	}
	wg.Wait()
	if started != 3 || j.spent > 0.35+1e-9 {
		t.Errorf("%d calls started, $%.2f spent; want 3 calls within $0.35", started, j.spent)
	}
	if err := j.stopErr(); err == nil || !strings.Contains(err.Error(), "cost budget reached") {
		t.Errorf("stop = %v, want the cost budget", err)
	}
}

// A backend that reports no cost is not limited by the budget, nor held to
// one call at a time.
func TestCostBudgetIgnoresUnpricedBackends(t *testing.T) {
	var running, most atomic.Int32
	m := completionFunc(func(llm.Request) (*llm.Reply, error) {
		n := running.Add(1)
		for {
			old := most.Load()
			if n <= old || most.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		running.Add(-1)
		return &llm.Reply{JSON: "{}"}, nil
	})
	j, err := newCallJournal(m, t.TempDir(), Options{MaxCost: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	j.Complete(context.Background(), llm.Request{System: "first"})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := j.Complete(context.Background(), llm.Request{System: "next"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if most.Load() < 2 {
		t.Errorf("at most %d call in flight; want parallel calls", most.Load())
	}
}
