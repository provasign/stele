package formalize

import (
	"strings"
	"testing"

	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

func TestRuleSettingAWorldValueBecomesAReset(t *testing.T) {
	p := vocab.Proposal{
		Entities: []vocab.Entity{{Name: "Session", Attributes: []vocab.Attribute{
			{Name: "minutesLeft", Type: "int", Min: 0, Max: 60, SetBy: "world"},
		}}},
		Operations: []vocab.Operation{{Name: "Session.activity", Default: "allow"}},
	}
	reg, err := vocab.Load(vocab.Options{Context: "c"}, p)
	if err != nil {
		t.Fatal(err)
	}
	cands := []translate.Candidate{{RuleID: "r1", Outcome: "rule", Kind: "invariant", On: "Session.activity", Assert: "self.minutesLeft = 60"}}
	out, added := inferWrites(reg, p, parseRules(reg, cands))
	o := out.Operations[0]
	if strings.Join(o.Writes, ",") != "Session.minutesLeft" || o.Effect["minutesLeft"] != "60" || len(added) != 1 {
		t.Fatalf("operation = %+v, added %v", o, added)
	}
	if _, err := vocab.Load(vocab.Options{Context: "c"}, out); err != nil {
		t.Fatalf("the reset does not load: %v", err)
	}
	// A conditional rule is not a reset.
	cands[0].When = "self.minutesLeft < 5"
	if out, _ := inferWrites(reg, p, parseRules(reg, cands)); len(out.Operations[0].Writes) != 0 {
		t.Fatalf("conditional rule made a reset: %+v", out.Operations[0])
	}
}

func TestAliasRenameSkipsRulesThatDoNotResolve(t *testing.T) {
	p := vocab.Proposal{Entities: []vocab.Entity{{Name: "Workspace", Attributes: []vocab.Attribute{
		{Name: "idleWarningSent", Type: "bool"}, {Name: "idleWarningShown", Type: "bool"},
	}}}}
	reg, err := vocab.Load(vocab.Options{Context: "c"}, p)
	if err != nil {
		t.Fatal(err)
	}
	from, into := reg.Entities["Workspace"].Attrs["idleWarningShown"], reg.Entities["Workspace"].Attrs["idleWarningSent"]
	cands := []translate.Candidate{
		{RuleID: "bad", Outcome: "rule", Kind: "invariant", Assert: "Vm.idleCheck"}, // an unknown root
		{RuleID: "r1", Outcome: "rule", Kind: "invariant", Subject: "Workspace", Assert: "not self.idleWarningShown"},
	}
	out, err := renameAlias(reg, cands, from, into)
	if err != nil {
		t.Fatalf("one unresolvable rule blocked the merge: %v", err)
	}
	if out[0].Assert != "Vm.idleCheck" || out[1].Assert != "not workspace.idleWarningSent" {
		t.Fatalf("renamed = %+v", out)
	}
}
