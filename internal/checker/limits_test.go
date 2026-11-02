package checker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
)

func TestConflictLimitReportsIncompleteSearch(t *testing.T) {
	var src strings.Builder
	src.WriteString("context: c\nformatVersion: 1\n---\nentity: E\ncontext: c\nattributes:\n  value: { type: bool }\n")
	src.WriteString("---\nid: N\nkind: invariant\nassert: e.value = false\n")
	for i := 1; i <= 4; i++ {
		fmt.Fprintf(&src, "---\nid: P%d\nkind: invariant\nassert: e.value = true\n", i)
	}
	reg, err := ir.LoadDir(writeRegistry(t, src.String()))
	if err != nil {
		t.Fatal(err)
	}
	prog, err := lower.Lower(reg, lower.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{3, 4} {
		res, err := CheckWith(context.Background(), needZ3(t), prog, Options{MaxConflicts: limit})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Conflicts) != limit {
			t.Fatalf("limit %d: got %d conflicts", limit, len(res.Conflicts))
		}
		if incomplete := len(res.Undecided) > 0; incomplete != (limit == 3) {
			t.Errorf("limit %d: undecided=%v; must distinguish capped search from exact exhaustion", limit, res.Undecided)
		}
	}
}
