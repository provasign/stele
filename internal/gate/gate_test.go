package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/provasign/stele/internal/solver"
)

const vocab = `context: c
formatVersion: 1
---
extend: platform.Request
context: c
attributes:
  kind: { type: enum, values: [normal, erasure] }
---
entity: Resource
context: c
attributes:
  holds: { type: int, range: [0, 5] }
  status: { type: enum, values: [active, locked, deleted] }
---
operation: Resource.delete
context: c
default: allow
writes: [Resource.status]
`

const r12 = `---
id: R12
version: 3
kind: permission
effect: deny
operation: Resource.delete
when: self.holds > 0
`

const r20 = `---
id: R20
kind: invariant
on: Resource.delete
when: request.kind = normal
assert: self.status != deleted
`

func dir(t *testing.T, src string) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "r.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func gate(t *testing.T, base, head string) *Result {
	t.Helper()
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	b := ""
	if base != "" {
		b = dir(t, base)
	}
	res, err := Dirs(context.Background(), &solver.Z3{}, b, dir(t, head))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func hasError(res *Result, want string) bool {
	for _, e := range res.Errors {
		if strings.Contains(e, want) {
			return true
		}
	}
	return false
}

func TestNewRegistryIsChecked(t *testing.T) {
	if res := gate(t, "", vocab+r12+r20); !res.Accepted {
		t.Errorf("a consistent new registry was rejected: %+v", res)
	}
}

func TestConflictBlocksMerge(t *testing.T) {
	r31 := "---\nid: R31\nkind: permission\neffect: allow\noperation: Resource.delete\nwhen: request.kind = erasure\n"
	res := gate(t, vocab+r12, vocab+r12+r31)
	if res.Accepted || len(res.Check.Conflicts) != 1 {
		t.Fatalf("want one conflict blocking the merge, got %+v", res.Check)
	}
	d7 := "---\nid: D7\nresolves: [R31@1, R12@3]\nsupersede: { winner: R31, loser: R12, when: request.kind = erasure }\n"
	if res := gate(t, vocab+r12, vocab+r12+r31+d7); !res.Accepted {
		t.Errorf("the decision should resolve it: %+v", res.Check)
	}
}

// An open conflict tolerates exactly its pinned set; any other conflict still
// blocks, and editing a pinned rule makes the record tolerate nothing (D-044).
func TestOpenConflictRatchet(t *testing.T) {
	r31 := "---\nid: R31\nkind: permission\neffect: allow\noperation: Resource.delete\nwhen: request.kind = erasure\n"
	oc := "---\nid: OC-1\nrules: [R31@1, R12@3]\nsituation: an erasure request on a resource with holds\nsince: 2026-10-25\n"
	res := gate(t, vocab+r12, vocab+r12+r31+oc)
	if !res.Accepted || len(res.Open) != 1 || res.Open[0].Status != OpenTolerated {
		t.Fatalf("the pinned conflict should be tolerated: %+v %+v", res.Check, res.Open)
	}
	if len(res.NeedsApproval) != 1 || res.NeedsApproval[0].Kind != "open-conflict-added" {
		t.Errorf("adding an open conflict needs approval: %+v", res.NeedsApproval)
	}

	r32 := "---\nid: R32\nkind: permission\neffect: allow\noperation: Resource.delete\nwhen: request.kind = normal\n"
	if res := gate(t, vocab+r12+r31+oc, vocab+r12+r31+r32+oc); res.Accepted || len(res.Check.Conflicts) != 1 || res.Check.Conflicts[0].Rules[1] != "R32" {
		t.Errorf("a new conflict must block: %+v", res.Check)
	}

	bumped := strings.Replace(strings.Replace(r12, "self.holds > 0", "self.holds > 1", 1), "version: 3", "version: 4", 1)
	if res := gate(t, vocab+r12+r31+oc, vocab+bumped+r31+oc); res.Accepted || res.Open[0].Status != OpenStale {
		t.Errorf("editing a pinned rule must make the record stale and block: %+v %+v", res.Check, res.Open)
	}

	retired := strings.Replace(r31, "kind:", "status: retired\nkind:", 1)
	if res := gate(t, vocab+r12+r31+oc, vocab+r12+retired+oc); !res.Accepted || res.Open[0].Status != OpenResolved {
		t.Errorf("a conflict that is gone should be reported resolved: %+v", res.Open)
	}

	if res := gate(t, vocab+r12+r31+oc, vocab+r12+retired); res.Accepted || !hasError(res, "OC-1 was deleted") {
		t.Errorf("deleting an open conflict must be rejected: %+v", res.Errors)
	}
}

func TestRulesAreRetiredNotDeleted(t *testing.T) {
	if res := gate(t, vocab+r12+r20, vocab+r20); res.Accepted || !hasError(res, "R12 was deleted") {
		t.Errorf("deleting a rule must be rejected: %+v", res.Errors)
	}
	retired := strings.Replace(r12, "when:", "status: retired\nwhen:", 1)
	if res := gate(t, vocab+r12+r20, vocab+retired+r20); !res.Accepted {
		t.Errorf("retiring is the way to remove a rule: %+v", res.Errors)
	}
}

func TestEditNeedsVersionBump(t *testing.T) {
	edited := strings.Replace(r12, "self.holds > 0", "self.holds > 1", 1)
	if res := gate(t, vocab+r12, vocab+edited); res.Accepted || !hasError(res, "without a version bump") {
		t.Errorf("an edit without a bump must be rejected: %+v", res.Errors)
	}
	bumped := strings.Replace(edited, "version: 3", "version: 4", 1)
	if res := gate(t, vocab+r12, vocab+bumped); !res.Accepted {
		t.Errorf("a bumped edit should pass: %+v", res.Errors)
	}
}

func TestEditReopensDecision(t *testing.T) {
	r31 := "---\nid: R31\nkind: permission\neffect: allow\noperation: Resource.delete\nwhen: request.kind = erasure\n"
	d7 := "---\nid: D7\nresolves: [R31@1, R12@3]\nsupersede: { winner: R31, loser: R12, when: request.kind = erasure }\n"
	bumped := strings.Replace(strings.Replace(r12, "self.holds > 0", "self.holds > 1", 1), "version: 3", "version: 4", 1)
	if res := gate(t, vocab+r12+r31+d7, vocab+bumped+r31+d7); res.Accepted || !hasError(res, "stale") {
		t.Errorf("editing a rule a decision pins must re-open the decision: %+v", res.Errors)
	}
}

// Adding an enum value changes what `status != deleted` allows (D-019).
func TestEnumAdditionNeedsApproval(t *testing.T) {
	grown := strings.Replace(vocab, "[active, locked, deleted]", "[active, locked, deleted, archived]", 1)
	res := gate(t, vocab+r12+r20, grown+r12+r20)
	if len(res.NeedsApproval) != 1 || res.NeedsApproval[0].Symbol != "Resource.status" || strings.Join(res.NeedsApproval[0].Affected, ",") != "R20" {
		t.Errorf("want Resource.status flagged with R20 affected, got %+v", res.NeedsApproval)
	}
}

func TestNewAttributeIsConservative(t *testing.T) {
	grown := strings.Replace(vocab, "  holds:", "  region: { type: enum, values: [us, eu] }\n  holds:", 1)
	res := gate(t, vocab+r12+r20, grown+r12+r20)
	if !res.Accepted || len(res.NeedsApproval) != 0 {
		t.Errorf("adding an attribute needs no approval and keeps the verdict: %+v", res)
	}
}

func TestBrokenReferenceIsAnError(t *testing.T) {
	shrunk := strings.Replace(vocab, "  holds: { type: int, range: [0, 5] }\n", "", 1)
	if res := gate(t, vocab+r12, shrunk+r12); res.Accepted || len(res.Errors) == 0 {
		t.Errorf("removing an attribute a rule reads must fail to compile: %+v", res)
	}
}

// A step that creates an entity carries nothing over from an earlier state
// (createdBy): a clash only an existing instance could cause goes, a clash
// in the creation itself stays.
func TestCreatedBy(t *testing.T) {
	const cluster = `context: c
formatVersion: 1
---
entity: Cluster
context: c
attributes:
  tenant: { type: enum, values: [a, b, default] }
  backupTenant: { type: enum, values: [a, b, default] }
  backedUp: { type: bool }
---
operation: Cluster.init
context: c
default: allow
writes: [Cluster.tenant]
---
id: key
kind: invariant
when: self.backedUp
assert: self.backupTenant = self.tenant
---
id: tenant
kind: invariant
on: Cluster.init
assert: self.tenant = default
`
	if res := gate(t, "", cluster); res.Accepted {
		t.Fatalf("init on a cluster with a backup should break the backup key: %+v", res.Check)
	}
	created := "---\nextend: Cluster\ncontext: c\ncreatedBy: [Cluster.init]\n"
	if res := gate(t, "", cluster+created); !res.Accepted {
		t.Fatalf("a created cluster has no earlier backup: %+v %v", res.Check, res.Errors)
	}
	clash := "---\nid: backup\nkind: invariant\non: Cluster.init\nassert: self.backedUp and self.backupTenant = a\n"
	if res := gate(t, "", cluster+created+clash); res.Accepted {
		t.Fatalf("a clash in the creation itself must remain: %+v", res.Check)
	}
	if res := gate(t, "", cluster+"---\nextend: Cluster\ncontext: c\ncreatedBy: [Cluster.nope]\n"); !hasError(res, "no such operation") {
		t.Fatalf("unknown creator: %+v", res.Errors)
	}
}

// A check the solver leaves undecided is refused, unless the owner approved
// every undecided step knowing it stays unchecked.
func TestUncheckedStepsAccepted(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	// A solver that settles nothing: what it cannot decide changes as the
	// checker learns (the exact day count of ISSUES 80 is decided now).
	fake := filepath.Join(t.TempDir(), "z3")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'Z3 version 0 - fake'; exit 0; fi\ncat >/dev/null\necho unknown\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	z := &solver.Z3{Path: fake, Timeout: time.Second}
	d := dir(t, `context: c
formatVersion: 1
---
entity: Tool
context: c
attributes:
  lentAt: { type: time }
  daysOverdue: { type: int }
---
operation: Tool.overdue
context: c
default: allow
writes: [Tool.daysOverdue]
---
id: D
kind: invariant
on: Tool.overdue
when: now > self.lentAt + 14d
assert: self.lentAt + 14d + days(self.daysOverdue) <= now and now < self.lentAt + 14d + days(self.daysOverdue + 1)
`)
	res, err := Dirs(context.Background(), z, "", d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted || !res.OnlyUndecided() {
		t.Fatalf("accepted %v, verdict %v; want the step undecided", res.Accepted, res.Check.Verdict)
	}
	var docs string
	for _, u := range res.Check.Undecided {
		docs += "---\nunchecked: " + u + "\nby: owner\nat: \"2026-11-03\"\n"
	}
	if err := os.WriteFile(filepath.Join(d, "unchecked.yaml"), []byte(docs), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err = Dirs(context.Background(), z, "", d); err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || len(res.Unchecked) == 0 {
		t.Fatalf("accepted %v unchecked %v; want accepted, naming the unchecked steps", res.Accepted, res.Unchecked)
	}
}

// TestRemovedEntityOfAWrite: a head that removes an entity together with
// another entity's operation writing it is gated, not a nil dereference (a
// code review, 2026-11-03).
func TestRemovedEntityOfAWrite(t *testing.T) {
	base := `context: c
formatVersion: 1
---
entity: Door
context: c
attributes:
  locked: { type: bool }
---
entity: Log
context: c
attributes:
  written: { type: bool }
---
operation: Door.close
context: c
default: allow
writes: [Door.locked, Log.written]
---
id: R1
kind: invariant
on: Door.close
assert: self.locked
`
	head := `context: c
formatVersion: 1
---
entity: Door
context: c
attributes:
  locked: { type: bool }
---
operation: Door.close
context: c
default: allow
writes: [Door.locked]
---
id: R1
kind: invariant
on: Door.close
assert: self.locked
`
	res := gate(t, base, head)
	if len(res.Errors) > 0 {
		t.Fatalf("errors %v", res.Errors)
	}
}
