package vocab

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
)

func TestSimilarPairs(t *testing.T) {
	used := []Used{
		{Name: "Mailer.reminderEnabled", Text: "Remind the holder a day before the reservation expires."},
		{Name: "Reservation.reminderSent", Text: "MUST NOT send reminders to opted-out users."},
		{Name: "Invoice.total", Text: "An invoice total MUST equal the sum of its lines."},
	}
	pairs := SimilarPairs(used, 2, 0.1)
	if len(pairs) != 1 || pairs[0] != [2]int{0, 1} {
		t.Errorf("want only the two reminder attributes paired, got %v", pairs)
	}
}

func TestApplyMerges(t *testing.T) {
	p := Proposal{
		Entities:   []Entity{{Name: "Order", Attributes: []Attribute{{Name: "paid", Type: "bool"}, {Name: "paymentState", Type: "enum", Values: []string{"none", "taken"}}, {Name: "charged", Type: "bool"}}}},
		Operations: []Operation{{Name: "Order.pay", Writes: []string{"Order.paid", "Order.charged", "Order.paymentState"}}},
	}
	out, applied := ApplyMerges(p, []Merge{
		{From: "Order.paid", Into: "Order.charged"},
		{From: "Order.charged", Into: "Order.paymentState"}, // the earlier target merges on: follow the chain
		{From: "Order.nothing", Into: "Order.paid"},         // not ours: ignored
	})
	if len(applied) != 2 || applied[0].Into != "Order.paymentState" {
		t.Fatalf("applied = %+v", applied)
	}
	if as := out.Entities[0].Attributes; len(as) != 1 || as[0].Name != "paymentState" {
		t.Errorf("attributes = %+v", as)
	}
	if w := strings.Join(out.Operations[0].Writes, ","); w != "Order.paymentState" {
		t.Errorf("writes = %s", w)
	}
}

// pairEcho answers each pairs request with a merge naming the first pair it
// was asked about, finishing in reverse order of arrival.
type pairEcho struct {
	mu      sync.Mutex
	calls   int
	release chan struct{}
}

func (p *pairEcho) Name() string { return "echo" }

func (p *pairEcho) Complete(_ context.Context, req llm.Request) (*llm.Reply, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	if n == 3 {
		close(p.release)
	}
	p.mu.Unlock()
	<-p.release // all three are in flight before any answers
	first := strings.TrimSuffix(strings.Fields(req.Turns[0].Text[strings.Index(req.Turns[0].Text, "\npair "):])[1], ":")
	time.Sleep(time.Duration(4-n) * 10 * time.Millisecond)
	return &llm.Reply{JSON: `{"merges":[{"from":"p` + first + `","into":"x","reason":""}],"links":[]}`}, nil
}

func TestConsolidatePairsParallel(t *testing.T) {
	used := make([]Used, 76)
	for i := range used {
		used[i] = Used{Name: fmt.Sprintf("E.a%d", i), Type: "bool"}
	}
	var pairs [][2]int
	for i := 0; i < 75; i++ {
		pairs = append(pairs, [2]int{i, i + 1})
	}
	m := &pairEcho{release: make(chan struct{})}
	rep := &Report{}
	merges, _, err := ConsolidatePairs(context.Background(), m, &ir.Registry{}, used, pairs, 60, 3, rep)
	if err != nil || m.calls != 3 || rep.Rounds != 3 {
		t.Fatalf("calls %d rounds %d err %v", m.calls, rep.Rounds, err)
	}
	// 75 pairs over 3 calls is 25 each, raised to the floor of 30: 1, 31, 61.
	var got []string
	for _, mg := range merges {
		got = append(got, mg.From)
	}
	if strings.Join(got, " ") != "p1 p31 p61" {
		t.Errorf("answers not combined in pair order: %v", got)
	}
}
