package change

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/llm"
)

// spy counts the requests that propose facts over all the statements.
type spy struct {
	fake
	facts *int
}

func (s spy) Complete(ctx context.Context, req llm.Request) (*llm.Reply, error) {
	if strings.Contains(req.System, "find the facts the requirements talk about") {
		*s.facts++
	}
	return s.fake.Complete(ctx, req)
}

// Statements that fit one translation request see each other; more are
// sent as several requests at once, so their facts are proposed first.
func TestFactsFirstWhenSeveralRequests(t *testing.T) {
	for _, tc := range []struct{ n, want int }{{translateBatch, 0}, {translateBatch + 1, 1}} {
		delta := "## ADDED Requirements\n"
		for i := 1; i <= tc.n; i++ {
			delta += fmt.Sprintf("\n### Requirement: Shelf %d\nShelf %d MUST be labelled.\n", i, i)
		}
		repo, registry := setup(t, delta)
		facts := 0
		res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: t.TempDir(),
			Clients: []llm.Client{spy{one("labelled", map[string]any{"kind": "informal", "rationale": "a label"}), &facts}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Added) != tc.n {
			t.Fatalf("%d statements: added %d", tc.n, len(res.Added))
		}
		if (facts > 0) != (tc.want > 0) {
			t.Errorf("%d statements: %d facts request(s), want facts first = %v", tc.n, facts, tc.want > 0)
		}
	}
}
