package checker

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/oracle"
)

// Check every value pair against the independent IR interpreter, including
// reordered declarations, negation, and an operation's post-state.
func TestCompatibleEnumComparisonsMatchOracle(t *testing.T) {
	for _, op := range []string{"=", "!="} {
		for _, actual := range []string{"pending", "ready", "failed"} {
			for _, reported := range []string{"pending", "ready", "failed"} {
				t.Run(actual+op+reported, func(t *testing.T) {
					dir := writeRegistry(t, fmt.Sprintf(`context: c
formatVersion: 1
---
entity: Log
context: c
attributes:
  actual: { type: enum, values: [pending, ready, failed] }
  reported: { type: enum, values: [failed, pending, ready] }
---
operation: Log.copy
context: c
default: allow
writes: [Log.reported]
---
id: Copy
kind: invariant
on: Log.copy
when: self.actual = %s
assert: self.reported %s self.actual
---
id: Fixed
kind: invariant
on: Log.copy
when: self.actual = %s
assert: self.reported = %s
`, actual, op, actual, reported))
					var want [][]string
					if (actual == reported) != (op == "=") {
						want = [][]string{{"Copy", "Fixed"}}
					}
					got := check(t, dir, lower.Options{})
					var conflicts [][]string
					for _, c := range got.Conflicts {
						conflicts = append(conflicts, c.Rules)
					}
					if !reflect.DeepEqual(conflicts, want) {
						t.Fatalf("SMT conflicts %v, want %v", conflicts, want)
					}
					reg, err := ir.LoadDir(dir)
					if err != nil {
						t.Fatal(err)
					}
					model, err := oracle.Build(reg, true)
					if err != nil {
						t.Fatal(err)
					}
					reference, err := model.MinimalConflictSets("Log.copy")
					if err != nil || !reflect.DeepEqual(reference, want) {
						t.Fatalf("oracle conflicts %v, want %v; %v", reference, want, err)
					}
				})
			}
		}
	}
}

func TestIncompatibleEnumDomainsRemainRejected(t *testing.T) {
	for _, values := range []string{"[ready, stopped]", "[ready]", "[ready, failed, pending]"} {
		dir := writeRegistry(t, `context: c
formatVersion: 1
---
entity: Log
context: c
attributes:
  actual: { type: enum, values: [ready, failed] }
  reported: { type: enum, values: `+values+` }
---
id: R
kind: invariant
assert: log.actual = log.reported
`)
		reg, err := ir.LoadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lower.Lower(reg, lower.Options{}); err == nil || !strings.Contains(err.Error(), "incompatible enum domains") {
			t.Fatalf("accepted incompatible domains %s: %v", values, err)
		}
	}
}
