package change

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// A conflict's rules are checked against their own texts before the owner
// is asked. Reading a requirement again to settle a clash gave a new
// reading that clashed elsewhere: each round of a first adoption moved one
// or two clashes to other pairs (2026-11-10). A rule that does not say
// what its text says is instead set aside: the requirement keeps its other
// rules, the finding says what is no longer checked, and the conflicts can
// only become fewer.

// auditFile holds, for one change, the conflicts already audited and the
// rules set aside.
const auditFile = "audit.json"

// asideRule is a rule set aside: of statement Statement while its reading
// is the one cached under Key.
type asideRule struct {
	Rule      string `json:"rule"`
	Statement string `json:"statement"`
	Key       string `json:"key"`
	Reading   string `json:"reading"`
	Reason    string `json:"reason"`
}

type auditState struct {
	// Checked maps a conflict's requirements and rules to the rules of it
	// found misread, so a conflict is audited once.
	Checked map[string][]string `json:"checked,omitempty"`
	Aside   []asideRule         `json:"set_aside,omitempty"`
	// Reread holds, by statement, the note it is read again with once
	// before any of its rules is set aside: the audit's reasons. Set aside
	// at once, a steward's "MUST be allowed to hold up to 5 tools" lost
	// both its readings, one on the audit's own guess about who acts, and
	// the contradiction it was in never reached the owner (ISSUES 140).
	Reread map[string]auditNote `json:"reread,omitempty"`
}

// auditNote is a note for a statement while its text hashes to Hash.
type auditNote struct {
	Hash string `json:"hash"`
	Note string `json:"note"`
}

// auditNotes is the audit's note for s, while its text is the one audited.
func auditNotes(dir string, s translate.Statement) []string {
	if n, ok := loadAudit(dir).Reread[s.ID]; ok && n.Hash == translate.TextHash(s.Prose) {
		return []string{n.Note}
	}
	return nil
}

func loadAudit(dir string) auditState {
	st := auditState{Checked: map[string][]string{}}
	if src, err := os.ReadFile(filepath.Join(dir, auditFile)); err == nil {
		_ = json.Unmarshal(src, &st)
	}
	if st.Checked == nil {
		st.Checked = map[string][]string{}
	}
	return st
}

func (st auditState) save(dir string) error {
	src, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, auditFile), append(src, '\n'), 0o644)
}

// setAside removes from cands the rules the audit set aside, while their
// statement's reading is the one audited, and says so in the statement's
// findings. A statement left with no rule is recorded as unchecked.
func setAside(cands []translate.Candidate, st auditState, c *cache, res *Result) []translate.Candidate {
	if len(st.Aside) == 0 {
		return cands
	}
	left := map[string]int{} // rules a statement keeps
	gone := map[string][]asideRule{}
	out := cands[:0:0]
	for _, cand := range cands {
		if cand.Outcome != "rule" {
			out = append(out, cand)
			continue
		}
		k := slices.IndexFunc(st.Aside, func(a asideRule) bool {
			return a.Rule == cand.RuleID && a.Statement == cand.StatementID && a.Key == c.Entries[cand.StatementID].Key
		})
		if k < 0 {
			left[cand.StatementID]++
			out = append(out, cand)
			continue
		}
		gone[cand.StatementID] = append(gone[cand.StatementID], st.Aside[k])
	}
	for id, rules := range gone {
		if res.Findings == nil {
			res.Findings = map[string][]string{}
		}
		for _, a := range rules {
			res.Findings[id] = append(slices.Clone(res.Findings[id]), fmt.Sprintf("no rule checks what Stele first read as %q: it clashed with another requirement and does not say what the text says (%s)", a.Reading, a.Reason))
			res.SetAside++
		}
		if left[id] == 0 && !slices.ContainsFunc(out, func(x translate.Candidate) bool { return x.StatementID == id }) {
			out = append(out, translate.Candidate{StatementID: id, Outcome: "informal", Rationale: "Stele's reading of it clashed with another requirement and did not say what the text says (" + rules[0].Reason + "); recorded without rules"})
		}
	}
	return out
}

// audit checks the rules of each conflict the owner would be asked about
// against their own requirements' texts, in one model call, and sets aside
// those that do not say what the text says. Only rules of requirements in
// the change can be set aside, and none of a requirement the owner already
// corrected in their own words. It returns how many it set aside.
func audit(ctx context.Context, opt Options, res *Result, head *ir.Registry, c *cache, as []Answer) int {
	if len(opt.Clients) == 0 || opt.NoAudit {
		return 0
	}
	if opt.MaxCost > 0 && res.Usage.CostUSD >= opt.MaxCost {
		return 0
	}
	inChange := map[string]bool{}
	for _, id := range append(append([]string{}, res.Added...), res.Modified...) {
		inChange[id] = true
	}
	guided := map[string]bool{} // read at the owner's own word
	for _, a := range as {
		if a.By != "agent" && (a.Kind == "reread" || a.Choice == "misread") {
			for id := range a.Statements {
				guided[id] = true
			}
		}
	}
	st := loadAudit(opt.Out)
	type item struct {
		key   string
		rules map[string]*ir.Rule
	}
	var items []item
	var b strings.Builder
	for _, q := range res.Questions {
		if q.Kind != "conflict" || !q.AskOwner || len(q.rules) == 0 {
			continue
		}
		it := item{rules: map[string]*ir.Rule{}}
		var block strings.Builder
		if q.Example != "" {
			fmt.Fprintf(&block, "Situation: %s\n", q.Example)
		}
		block.WriteString("Requirements:\n")
		for _, r := range q.Requirements {
			text, _, _ := strings.Cut(r.Text, "\nGlossary: ")
			fmt.Fprintf(&block, "- %s: %s\n", r.ID, llm.Data(strings.Join(strings.Fields(text), " ")))
		}
		block.WriteString("Rules (rule id: reading):\n")
		for _, id := range slices.Compact(slices.Sorted(slices.Values(q.rules))) {
			x := head.Rule(id)
			if x == nil || x.StatementID == "" {
				continue
			}
			fmt.Fprintf(&block, "- %s (of %s): %s\n", id, x.StatementID, english(x))
			if inChange[x.StatementID] && !guided[x.StatementID] {
				it.rules[id] = x
			}
		}
		if len(it.rules) == 0 {
			continue
		}
		h := sha256.Sum256([]byte(block.String()))
		it.key = hex.EncodeToString(h[:8])
		if _, done := st.Checked[it.key]; done {
			continue
		}
		fmt.Fprintf(&b, "## Conflict %s\n%s\n", it.key, block.String())
		items = append(items, it)
	}
	if len(items) == 0 {
		return 0
	}
	if opt.Log != nil {
		fmt.Fprintf(opt.Log, "audit: checking the rules of %d conflict(s) against their requirements' texts ...\n", len(items))
	}
	cctx, cancel := opt.request(ctx)
	reply, err := opt.Clients[0].Complete(cctx, llm.Request{System: auditPrompt, Turns: []llm.Turn{{Role: "user", Text: b.String()}}, Schema: auditSchema})
	cancel()
	if err != nil {
		res.Warnings = append(res.Warnings, "audit of the conflicts' rules: "+err.Error())
		return 0
	}
	res.Usage.Add(reply.Usage)
	var out struct {
		Conflicts []struct {
			ID    string `json:"id"`
			Rules []struct {
				Rule    string `json:"rule"`
				Verdict string `json:"verdict"`
				Reason  string `json:"reason"`
			} `json:"rules"`
		} `json:"conflicts"`
	}
	if err := json.Unmarshal([]byte(reply.JSON), &out); err != nil {
		res.Warnings = append(res.Warnings, "audit of the conflicts' rules: the reply is not the expected JSON: "+err.Error())
		return 0
	}
	added := 0
	reread := map[string][]string{} // statement -> reasons, read again once
	forget := map[string]bool{}     // conflicts to judge again after the reread
	for _, it := range items {
		st.Checked[it.key] = []string{}
		k := slices.IndexFunc(out.Conflicts, func(x struct {
			ID    string `json:"id"`
			Rules []struct {
				Rule    string `json:"rule"`
				Verdict string `json:"verdict"`
				Reason  string `json:"reason"`
			} `json:"rules"`
		}) bool {
			return x.ID == it.key
		})
		if k < 0 {
			delete(st.Checked, it.key) // not judged: asked again next time
			continue
		}
		for _, v := range out.Conflicts[k].Rules {
			x := it.rules[v.Rule]
			if x != nil && v.Verdict == "unsure" && opt.Log != nil {
				fmt.Fprintf(opt.Log, "audit: unsure of %s: %s\n", v.Rule, strings.TrimSpace(v.Reason))
			}
			if x == nil || v.Verdict != "misread" || strings.TrimSpace(v.Reason) == "" {
				continue
			}
			st.Checked[it.key] = append(st.Checked[it.key], v.Rule)
			if n, ok := st.Reread[x.StatementID]; !ok || n.Hash != translate.TextHash(res.Statements[x.StatementID]) {
				reason := strings.TrimSuffix(strings.TrimSpace(v.Reason), ".")
				reread[x.StatementID] = append(reread[x.StatementID], fmt.Sprintf("%q: %s", x.Words(), reason))
				// Judged again after the reread, also when it reads the
				// same: misread again, it is then set aside.
				forget[it.key] = true
				continue
			}
			a := asideRule{Rule: x.ID, Statement: x.StatementID, Key: c.Entries[x.StatementID].Key, Reading: english(x), Reason: strings.TrimSuffix(strings.TrimSpace(v.Reason), ".")}
			if a.Key == "" || slices.ContainsFunc(st.Aside, func(o asideRule) bool { return o.Rule == a.Rule && o.Statement == a.Statement && o.Key == a.Key }) {
				continue
			}
			st.Aside = append(st.Aside, a)
			added++
			if opt.Log != nil {
				fmt.Fprintf(opt.Log, "audit: set aside %s: %s\n", a.Rule, a.Reason)
			}
		}
	}
	for key := range forget {
		delete(st.Checked, key)
	}
	if len(reread) > 0 && st.Reread == nil {
		st.Reread = map[string]auditNote{}
	}
	for _, id := range slices.Sorted(maps.Keys(reread)) {
		st.Reread[id] = auditNote{Hash: translate.TextHash(res.Statements[id]), Note: "Stele's first reading of this requirement clashed with another requirement, and a check against the text found it does not say what the text says: " +
			strings.Join(reread[id], "; ") + ". Read it again saying what the text says, no more and no less: keep each condition the text gives, add none, and where it names who a condition is about (the one who performs the step, the one it is done for, the item it acts on), read that one."}
		added++
		if opt.Log != nil {
			fmt.Fprintf(opt.Log, "audit: reading %s again: %s\n", id, strings.Join(reread[id], "; "))
		}
	}
	if err := st.save(opt.Out); err != nil {
		res.Warnings = append(res.Warnings, "audit of the conflicts' rules: "+err.Error())
		return 0
	}
	return added
}

const auditPrompt = `Software requirements were translated, one by one, into rules a solver checks. The solver found rules that cannot all hold in some situation. Before the project's owner is asked which requirement is right, check each rule against the text of its own requirement, and only that.

A rule is read like this: "invariant when C then A" holds A in every state where C holds; "invariant on Step when C then A" says that when Step runs from a state where C holds, A holds after it; "permission allow Step when C" says Step must run whenever C holds; "permission deny Step when C" says it must not; "permission possible Step when C" says Step must be able to run in some situation where C holds; "precondition Step when C" says Step must not run unless C holds.

Who a rule is about: "self" is the item the step acts on (the tool lent, the order shipped); "actor" is whoever invokes the step (for a borrowing, the member who borrows, unless the text says someone else hands it out); "request" is what the step was asked with, often naming the party it is for (request.email). When the text does not say who performs the step, a condition on the actor and one on the party the request names are both fair readings of "a steward may hold", "a member may borrow": that is not a misreading. Give "unsure" when the verdict hangs only on who performs the step and the text does not say. Judge every other point apart: a rule that requires a step to run where the text grants a permission, or adds a condition the text does not give, is "misread" whoever performs the step.

For each rule give a verdict:
- "says": the rule states what the text states. Two faithful rules that clash are a real disagreement or a gap in the specs, which is the owner's to settle: both are "says". A requirement stated without a condition is faithfully read without one.
- "misread": the rule does not state what the text states: its condition is turned round or is one the text does not give, it requires a different thing or of a different fact, it applies in situations the text does not speak of, or it requires a step to run where the text only says what the step does.
- "unsure": you cannot tell from the text, or it hangs only on who performs the step and the text does not say.

Judge only the rule against its own text. Never mark a rule "misread" because it clashes with another, and never say which requirement should win. Reply for every conflict, by its id, with every rule listed under it.`

const auditSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["conflicts"],
  "properties": {
    "conflicts": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["id", "rules"],
        "properties": {
          "id": {"type": "string"},
          "rules": {
            "type": "array",
            "items": {
              "type": "object",
              "additionalProperties": false,
              "required": ["rule", "verdict", "reason"],
              "properties": {"rule": {"type": "string"}, "verdict": {"type": "string", "enum": ["says", "misread", "unsure"]}, "reason": {"type": "string"}}
            }
          }
        }
      }
    }
  }
}`
