package change

import (
	"reflect"
	"testing"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/gate"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
)

// TestConfirmBlocking: a "blocks a step" finding from translation is kept
// only while the head registry's check still shows the rule alone leaving
// a step without an outcome; other findings are kept.
func TestConfirmBlocking(t *testing.T) {
	head := &ir.Registry{Rules: []*ir.Rule{{ID: "S1.1", StatementID: "S1"}, {ID: "S2.1", StatementID: "S2"}}}
	res := &Result{
		Gate: &gate.Result{Check: &checker.Result{Conflicts: []checker.Conflict{{Unit: "Rental.end", Rules: []string{"S2.1"}}}}},
		Findings: map[string][]string{
			"S1": {"blocks a step: rule [S1.1] leaves step Rental.end", `no rule checks "the day rate"`},
			"S2": {"blocks a step: rule [S2.1] leaves step Rental.end"},
			"S3": {"blocks a step: rule [S3.1] leaves step Rental.end"},
		},
	}
	confirmBlocking(res, head)
	want := map[string][]string{
		"S1": {`no rule checks "the day rate"`},
		"S2": {"blocks a step: rule [S2.1] leaves step Rental.end"},
	}
	if !reflect.DeepEqual(res.Findings, want) {
		t.Errorf("findings = %q, want %q", res.Findings, want)
	}
}

// TestApprovedAlready: a change whose every requirement already has
// approved rules for its exact text is reported approved, not checked
// against itself; a changed text or an unapproved rule is not.
func TestApprovedAlready(t *testing.T) {
	text := "Requirement: Locked doors\nThe door MUST be locked at night."
	h := translate.TextHash(text)
	approved := &ir.Rule{ID: "R1", StatementID: "doors/Locked doors", Status: "active", TextHash: h, ApprovedBy: "Dana", ApprovedHash: h}
	reg := &ir.Registry{Rules: []*ir.Rule{approved}}
	if !approvedAlready(reg, map[string]string{"doors/Locked doors": text}) {
		t.Error("an approved change was not recognized")
	}
	if approvedAlready(reg, map[string]string{"doors/Locked doors": text + " And at dawn."}) {
		t.Error("an edited text passed as approved")
	}
	unapproved := *approved
	unapproved.ApprovedBy = ""
	if approvedAlready(&ir.Registry{Rules: []*ir.Rule{&unapproved}}, map[string]string{"doors/Locked doors": text}) {
		t.Error("an unapproved rule passed as approved")
	}
	if approvedAlready(reg, map[string]string{"doors/Other": text}) {
		t.Error("a requirement with no rules passed as approved")
	}
}
