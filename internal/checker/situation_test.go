package checker

import (
	"strings"
	"testing"
)

// TestSlotsInSituation: the values of a relation's slots each keep their
// own name. Cut at "#", every tool's id merged into one value, and the
// example of a clash between tools sharing an id described only which
// borrowers were unset (a developer trial, 2026-11-03).
func TestSlotsInSituation(t *testing.T) {
	bs := []Binding{
		{Name: "Shed.tools#1.id", Value: "0"},
		{Name: "Shed.tools#1:in", Value: "true"},
		{Name: "Shed.tools#2.id", Value: "0"},
		{Name: "Shed.tools#2:in", Value: "true"},
		{Name: "Shed.tools#3:in", Value: "false"},
		{Name: "Shed.tools#2.borrowerEmail?", Value: "false"},
		{Name: "Tool.*instances#1.id", Value: "4"},
		{Name: "Result.ids#n", Value: "1"},
		{Name: "Result.ids#0", Value: "7"},
	}
	got := Plain(Situation(nil, bs))
	for _, want := range []string{
		"shed: tools #1: id is 0", "shed: tools #1 exists", "shed: tools #2: id is 0",
		"no shed: tools #3", "shed: tools #2: borrower email is unset", "tool: other #1: id is 4", "result: ids is [7]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}
