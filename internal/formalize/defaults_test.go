package formalize

import (
	"testing"

	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// An operation the proposal gives default deny opens to allow when its rules
// only restrict it; one that some rule allows, by name or by pattern, or
// that no rule mentions, keeps its default.
func TestRestrictedDefaults(t *testing.T) {
	p := vocab.Proposal{Operations: []vocab.Operation{
		{Name: "Order.cancel", Default: "deny"},
		{Name: "Order.refund", Default: "deny"},
		{Name: "Job.run", Default: "deny"},
		{Name: "Order.archive", Default: "deny"},
		{Name: "Order.ship", Default: "allow"},
	}}
	rule := func(id, kind, effect, op string) parsedRule {
		return parsedRule{cand: &translate.Candidate{RuleID: id, Kind: kind, Effect: effect, Operation: op}}
	}
	rules := []parsedRule{
		rule("C1", "permission", "deny", "Order.cancel"),
		rule("C2", "precondition", "", "Order.cancel"),
		rule("R1", "permission", "deny", "Order.refund"),
		rule("R2", "permission", "allow", "Order.refund"),
		rule("J1", "permission", "deny", "Job.run"),
		rule("A1", "permission", "possible", "Job.*"),
	}
	out, changed := restrictedDefaults(p, rules)
	want := map[string]string{"Order.cancel": "allow", "Order.refund": "deny", "Job.run": "deny", "Order.archive": "deny", "Order.ship": "allow"}
	for _, o := range out.Operations {
		if o.Default != want[o.Name] {
			t.Errorf("%s: default %s, want %s", o.Name, o.Default, want[o.Name])
		}
	}
	if len(changed) != 1 || changed[0] != "Order.cancel: default allow, since its rules only restrict it (C1, C2)" {
		t.Errorf("changed = %q", changed)
	}
	if p.Operations[0].Default != "deny" {
		t.Error("the input proposal was modified")
	}
}
