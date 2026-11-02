package gate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/checker"
)

func TestOpenConflictsDoNotConsumeEnumerationLimit(t *testing.T) {
	var src strings.Builder
	src.WriteString("context: c\nformatVersion: 1\n---\nentity: E\ncontext: c\nattributes:\n  value: { type: bool }\n")
	src.WriteString("---\nid: N\nkind: invariant\nassert: e.value = false\n")
	count := checker.MaxConflicts + 1
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&src, "---\nid: P%d\nkind: invariant\nassert: e.value = true\n", i)
		fmt.Fprintf(&src, "---\nid: OC-%d\nrules: [N@1, P%d@1]\nsituation: existing contradiction\nsince: 2026-10-25\n", i, i)
	}
	baseline := src.String()
	res := gate(t, "", baseline)
	if !res.Accepted || len(res.Check.Undecided) != 0 || len(res.Open) != count {
		t.Fatalf("known conflicts must not exhaust the budget: %+v", res)
	}
	for _, o := range res.Open {
		if o.Status != OpenTolerated {
			t.Errorf("existing conflict %s incorrectly marked %s", o.ID, o.Status)
		}
	}
	head := baseline + "---\nid: NEW\nkind: invariant\nassert: e.value = true\n"
	res = gate(t, baseline, head)
	if res.Accepted || res.Check == nil || len(res.Check.Conflicts) != 1 || strings.Join(res.Check.Conflicts[0].Rules, ",") != "N,NEW" {
		t.Fatalf("the new conflict must be found beyond the open ones: %+v", res)
	}
}
