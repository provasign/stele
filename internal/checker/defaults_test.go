package checker

import (
	"reflect"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/oracle"
)

// A shipped default must not hide a contradiction in another configuration.
// Compare the SMT result with the independent enumerating oracle in both modes.
func TestConfiguredDefaultDoesNotHideConflict(t *testing.T) {
	dir := writeRegistry(t, `
context: c
formatVersion: 1
---
entity: Config
context: c
attributes:
  readOnly: { type: bool }
---
entity: Store
context: c
attributes:
  saved: { type: bool }
---
operation: Store.save
context: c
default: allow
writes: [Store.saved]
---
id: D1
kind: derivation
assert: config.readOnly.default = false
---
id: R1
kind: permission
operation: Store.save
when: config.readOnly
effect: allow
---
id: R2
kind: permission
operation: Store.save
when: config.readOnly
effect: deny
`)
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, assume := range []bool{false, true} {
		name := "all configurations"
		var want [][]string
		if assume {
			name = "shipped configuration"
		} else {
			want = [][]string{{"R1", "R2"}}
		}
		t.Run(name, func(t *testing.T) {
			res := check(t, dir, lower.Options{AssumeDefaults: assume})
			var got [][]string
			for _, c := range res.Conflicts {
				got = append(got, c.Rules)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("SMT conflicts = %v, want %v", got, want)
			}
			if assume && res.Verdict != Pass {
				t.Errorf("configured defaults should pass: %+v", res)
			}
			model, err := oracle.BuildWith(reg, true, assume)
			if err != nil {
				t.Fatal(err)
			}
			got, err = model.MinimalConflictSets("Store.save")
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("oracle conflicts = %v, want %v (error %v)", got, want, err)
			}
		})
	}
}

// Two statements giving one setting different defaults conflict, in both
// modes; a third repeating one of them adds nothing. The registry loads, and
// the rest is still checked: a decision retiring one side clears it.
func TestConflictingDefaultsAreAConflict(t *testing.T) {
	const reg = `
context: c
formatVersion: 1
---
entity: Config
context: c
attributes:
  idleMinutes: { type: int, range: [1, 120] }
---
id: Old
kind: derivation
assert: config.idleMinutes.default = 30
---
id: New
kind: derivation
assert: config.idleMinutes.default = 15
---
id: Again
kind: derivation
assert: config.idleMinutes.default = 15
`
	for _, assume := range []bool{false, true} {
		got := check(t, writeRegistry(t, reg), lower.Options{AssumeDefaults: assume})
		var conflicts [][]string
		for _, c := range got.Conflicts {
			conflicts = append(conflicts, c.Rules)
		}
		if !reflect.DeepEqual(conflicts, [][]string{{"New", "Old"}}) {
			t.Errorf("assume defaults %v: conflicts %v", assume, conflicts)
		}
	}
	retired := reg + `---
id: D1
retire: [Old@1]
rationale: the newer spec sets the timeout
`
	if got := check(t, writeRegistry(t, retired), lower.Options{AssumeDefaults: true}); len(got.Conflicts) != 0 {
		t.Errorf("a decision retiring one default should clear the conflict: %+v", got.Conflicts)
	}
}
