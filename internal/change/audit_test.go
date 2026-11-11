package change

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/llm"
)

// auditing answers the audit of a conflict's rules with the verdicts it is
// given, one per audit, and every other request as its fake does.
type auditing struct {
	fake
	verdicts []string
	audits   *int
	reread   *bool
}

var ruleLine = regexp.MustCompile(`(?m)^- (.+) \(of (.+)\): `)

func (a auditing) Complete(ctx context.Context, req llm.Request) (*llm.Reply, error) {
	if req.System == auditPrompt {
		text := req.Turns[0].Text
		verdict := a.verdicts[min(*a.audits, len(a.verdicts)-1)]
		*a.audits++
		var out []map[string]any
		for _, block := range strings.Split(text, "## Conflict ")[1:] {
			key, _, _ := strings.Cut(block, "\n")
			var rules []map[string]string
			for _, m := range ruleLine.FindAllStringSubmatch(block, -1) {
				v := "says"
				if strings.Contains(m[2], "Erasure") {
					v = verdict
				}
				rules = append(rules, map[string]string{"rule": m[1], "verdict": v, "reason": "it applies to every request, where the text speaks of erasure requests"})
			}
			out = append(out, map[string]any{"id": key, "rules": rules})
		}
		js, _ := json.Marshal(map[string]any{"conflicts": out})
		return &llm.Reply{JSON: string(js)}, nil
	}
	if strings.Contains(req.Turns[len(req.Turns)-1].Text, "Stele's first reading of this requirement clashed") {
		*a.reread = true
	}
	return a.fake.Complete(ctx, req)
}

// TestAuditReadsAgainBeforeSettingAside: a conflict's rule the audit finds
// misread is read again once, told why, before it is set aside. Misread
// again, it is set aside and the requirement says what is no longer
// checked; read right the second time, the conflict goes to the owner
// (ISSUES 140).
func TestAuditReadsAgainBeforeSettingAside(t *testing.T) {
	for _, c := range []struct {
		verdicts []string
		owner    bool
	}{{[]string{"misread", "misread"}, false}, {[]string{"misread", "says"}, true}} {
		repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Erasure\nErasure requests MUST always delete the resource.\n")
		audits, reread := 0, false
		client := auditing{fake: one("Erasure", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "request.kind = erasure"}), verdicts: c.verdicts, audits: &audits, reread: &reread}
		res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: t.TempDir(), Clients: []llm.Client{client}})
		if err != nil {
			t.Fatal(err)
		}
		if !reread || audits != 2 {
			t.Fatalf("%v: read again %v, audits %d", c.verdicts, reread, audits)
		}
		asked := slices.ContainsFunc(res.Questions, func(q Question) bool { return q.Kind == "conflict" && q.AskOwner })
		if asked != c.owner {
			t.Fatalf("%v: conflict asked %v; questions %+v", c.verdicts, asked, res.Questions)
		}
		if !c.owner && !slices.ContainsFunc(res.Findings["res/Erasure"], func(f string) bool { return strings.Contains(f, "no rule checks what Stele first read") }) {
			t.Fatalf("%v: findings %v", c.verdicts, res.Findings)
		}
	}
}
