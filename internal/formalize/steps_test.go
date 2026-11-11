package formalize

import (
	"context"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
)

// TestStepsOfOneActMerge: two requests read in parallel named one act
// twice (Member.borrow, Tool.lend); the statement on the new name is read
// again on the other, so requirements about the act can meet.
func TestStepsOfOneActMerge(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	opt.NoCover, opt.NoSelfCheck, opt.NoConsolidate = true, true, false
	stmts[0].Prose, stmts[1].Prose = "A suspended member MUST NOT borrow tools.", "A tool under repair MUST NOT be lent."
	vocab := `"entities":[
	{"name":"Member","statement_ids":["s1"],"attributes":[{"name":"status","type":"enum","values":["active","suspended"],"min":0,"max":0,"set_by":"system"}],"relations":[]},
	{"name":"Tool","statement_ids":["s2"],"attributes":[{"name":"status","type":"enum","values":["available","underRepair"],"min":0,"max":0,"set_by":"system"}],"relations":[]}],
	"extensions":[],`
	first := `{` + vocab + `
	"operations":[{"name":"Member.borrow","statement_ids":["s1"],"default":"allow","writes":[],"rationale":""},{"name":"Tool.lend","statement_ids":["s2"],"default":"allow","writes":[],"rationale":""}],
	"rules":[
	{"statement_id":"s1","kind":"permission","operation":"Member.borrow","effect":"deny","when":"self.status = suspended"},
	{"statement_id":"s2","kind":"permission","operation":"Tool.lend","effect":"deny","when":"self.status = underRepair"}]}`
	again := `{"entities":[],"extensions":[],"operations":[],"rules":[
	{"statement_id":"s1","kind":"permission","operation":"Tool.lend","effect":"deny","when":"count(m in instances(Member): m.status = suspended) >= 1"}]}`
	asked, reread := false, false
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		text := req.Turns[len(req.Turns)-1].Text
		switch {
		case req.System == stepsPrompt:
			asked = true
			if !strings.Contains(text, "step: Member.borrow (new)") || !strings.Contains(text, "A suspended member") {
				t.Errorf("steps request:\n%s", text)
			}
			return &llm.Reply{JSON: `{"merges":[{"from":"Member.borrow","into":"Tool.lend","reason":"a member borrowing a tool is the tool being lent"}]}`}, nil
		case strings.Contains(text, "one act under two names"):
			reread = true
			return &llm.Reply{JSON: again}, nil
		case strings.Contains(text, "statement_id: s1"):
			return &llm.Reply{JSON: first}, nil
		}
		return &llm.Reply{JSON: "{}"}, nil
	})
	if _, err := Run(context.Background(), m, stmts, dir, opt); err != nil {
		t.Fatal(err)
	}
	if !asked || !reread {
		t.Fatalf("asked %v, read again %v", asked, reread)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range reg.Rules {
		if x.Operation != "Tool.lend" {
			t.Errorf("rule %s is still on %s", x.ID, x.Operation)
		}
	}
}
