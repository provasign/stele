package change

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// definitionsFile caches, per conflict, the world facts its requirements'
// glossary entries were read as.
const definitionsFile = "definitions.json"

// definitions reads the glossary entries that a conflict's requirements
// cite as world facts, before the owner is asked (ISSUES 62): a glossary
// that defines one term by others ("an account field change is a change of
// the account ID, role ARN or external ID") ties attributes the rules read
// separately, and a clash that comes only from leaving them apart is not
// the owner's question. Only conflicts are read, so the cost is bounded by
// them. It returns how many facts it added to the change's world facts.
func definitions(ctx context.Context, opt Options, res *Result, head *ir.Registry, c *cache, files []string, as []Answer) int {
	if len(opt.Clients) == 0 || len(files) == 0 {
		return 0
	}
	cached := map[string][]translate.Candidate{}
	if src, err := os.ReadFile(filepath.Join(opt.Out, definitionsFile)); err == nil {
		_ = json.Unmarshal(src, &cached)
	}
	added := 0
	for _, q := range res.Questions {
		if q.Kind != "conflict" || !q.AskOwner || len(q.rules) == 0 {
			continue
		}
		var entries []string
		for _, r := range q.Requirements {
			for _, e := range glossaryOf(r.Text) {
				if !slices.Contains(entries, e) {
					entries = append(entries, e)
				}
			}
		}
		// What the owner said cannot happen, as a fact the check can use:
		// as a note to the translation alone it changed no rule, and the
		// same clash came back with another example (a developer trial,
		// 2026-11-03).
		for _, e := range ownerFacts(as, head) {
			if !slices.Contains(entries, e) {
				entries = append(entries, e)
			}
		}
		if len(entries) == 0 {
			continue
		}
		var rules []string
		attrs := map[string]*ir.Attr{}
		for _, id := range slices.Compact(slices.Sorted(slices.Values(q.rules))) {
			x := head.Rule(id)
			if x == nil {
				continue
			}
			rules = append(rules, english(x))
			subject, err := head.SubjectOf(x)
			if err != nil {
				continue
			}
			for _, e := range []ir.Expr{x.When, x.Assert} {
				if e != nil {
					for _, a := range head.Reads(e, subject) {
						attrs[a.QName()] = a
					}
				}
			}
		}
		h := sha256.New()
		fmt.Fprintf(h, "%s\x00%s\x00%s", q.ID, strings.Join(entries, "\x00"), strings.Join(rules, "\x00"))
		key := hex.EncodeToString(h.Sum(nil))[:16]
		facts, ok := cached[key]
		if !ok {
			if opt.Log != nil {
				fmt.Fprintf(opt.Log, "definitions: reading %d glossary entr(ies) for %s\n", len(entries), q.ID)
			}
			var err error
			// Each model call has its own limit; given the run's context
			// alone, a stalled glossary call held the command until the run
			// timeout (a code review, 2026-11-04).
			cctx, cancel := opt.request(ctx)
			facts, err = define(cctx, opt.Clients[0], entries, rules, attrs, q.Example, res)
			cancel()
			if err != nil {
				res.Warnings = append(res.Warnings, "definitions for "+q.ID+": "+err.Error())
				continue
			}
			translate.Revalidate(files, facts)
			facts = slices.DeleteFunc(facts, func(f translate.Candidate) bool { return f.Error != "" })
			cached[key] = facts
		}
		for _, f := range facts {
			rejected := rejectedFact(as, f.RuleID, candFactKey(f))
			if rejected || slices.ContainsFunc(c.Links, func(k translate.Candidate) bool {
				return k.When == f.When && k.Assert == f.Assert && k.On == f.On && k.Assume == f.Assume
			}) {
				continue
			}
			c.Links = append(c.Links, f)
			added++
		}
	}
	if src, err := json.MarshalIndent(cached, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(opt.Out, definitionsFile), src, 0o644)
	}
	return added
}

// ownerFacts are what the owner said about the project: situations that
// cannot happen, from this change's answers and the project's lessons.
func ownerFacts(as []Answer, head *ir.Registry) []string {
	var out []string
	add := func(says string) {
		if says = strings.TrimSpace(says); says != "" && !slices.Contains(out, ownerSays+says) {
			out = append(out, ownerSays+says)
		}
	}
	for _, a := range as {
		if a.Kind == "conflict" && a.Choice == "impossible" && a.By != "agent" {
			add("This situation cannot happen in this project: " + a.Note) // the situation and the owner's words; its lesson keeps the words
		}
	}
	for _, l := range head.Lessons {
		if l.Kind == ir.LessonFact {
			add(l.Says)
		}
	}
	return out
}

const ownerSays = "The owner says: "

// glossaryOf is the glossary entries an imported statement carries (the
// Kiro importer appends them as "Glossary: Term: text | Term: text").
func glossaryOf(prose string) []string {
	var out []string
	for _, line := range strings.Split(prose, "\n") {
		if rest, ok := strings.CutPrefix(line, "Glossary: "); ok {
			for _, e := range strings.Split(rest, " | ") {
				if e = strings.TrimSpace(e); e != "" {
					out = append(out, e)
				}
			}
		}
	}
	return out
}

// define asks which of the glossary entries tie the clashing rules'
// attributes together, as world facts.
func define(ctx context.Context, m llm.Client, entries, rules []string, attrs map[string]*ir.Attr, example string, res *Result) ([]translate.Candidate, error) {
	var b strings.Builder
	b.WriteString("Glossary entries the requirements cite:\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "- %s\n", e)
	}
	b.WriteString("\nThe rules that clash:\n")
	for _, r := range rules {
		fmt.Fprintf(&b, "- %s\n", r)
	}
	if example != "" {
		fmt.Fprintf(&b, "\nThe situation in which they clash: %s\n", example)
	}
	b.WriteString("\nThe attributes the rules read:\n")
	names := make([]string, 0, len(attrs))
	for n := range attrs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		a := attrs[n]
		fmt.Fprintf(&b, "- %s: %s", n, a.TypeString())
		if a.Kind == ir.Enum {
			fmt.Fprintf(&b, " {%s}", strings.Join(a.Values, ", "))
		}
		if a.Meaning != "" {
			fmt.Fprintf(&b, " - %s", a.Meaning)
		}
		b.WriteString("\n")
	}
	reply, err := m.Complete(ctx, llm.Request{System: definePrompt, Turns: []llm.Turn{{Role: "user", Text: b.String()}}, Schema: defineSchema})
	if err != nil {
		return nil, err
	}
	res.Usage.Add(reply.Usage)
	var out struct {
		Facts []struct {
			Term   string `json:"term"`
			On     string `json:"on"`
			When   string `json:"when"`
			Assert string `json:"assert"`
		} `json:"facts"`
	}
	if err := json.Unmarshal([]byte(reply.JSON), &out); err != nil {
		return nil, fmt.Errorf("reply is not the expected JSON: %v", err)
	}
	var facts []translate.Candidate
	for _, f := range out.Facts {
		if f.Assert == "" {
			continue
		}
		h := sha256.Sum256([]byte(f.On + "\x00" + f.When + "\x00" + f.Assert))
		facts = append(facts, translate.Candidate{RuleID: "world-def-" + hex.EncodeToString(h[:4]), Outcome: "rule", Kind: ir.Invariant,
			Effect: "none", On: f.On, Assume: f.On != "", When: f.When, Assert: f.Assert, Rationale: factSource(f.Term) + f.Assert})
	}
	return facts, nil
}

const definePrompt = `A spec's glossary defines some terms by others ("Account_Field_Change: a change of the account ID, the role ARN or the external ID"). The requirements were translated into rules one by one, and a rule may read the defined term as one attribute while another reads the terms it is defined by as others. Nothing then ties them together, and a check may find a clash that comes only from that: a role ARN changed while "account field changed" is false.

You get the glossary entries the clashing requirements cite, the rules, the situation in which they clash and the attributes the rules read. For each glossary entry that defines what one of these attributes means by others of them, return the fact the definition states, as an invariant over those attributes:

- "assert" holds in every state the definition covers; "when" is empty or the condition under which the definition applies. Write paths exactly as the rules write them (self.x, request.x, entity.x), with =, !=, in {..}, not, and, or, implies.
- When the fact reads request.* or self.* of a step, set "on" to the step the rules name after "on" (UseCase.update); otherwise leave "on" empty.
- Use an equality (self.accountFieldChanged = (request.accountIdChanged or request.externalIdChanged)) only when the attributes cover every case the definition lists; when they cover some, write one implication per defining attribute (request.accountIdChanged implies self.accountFieldChanged).
- "term" is the glossary term.

An entry that starts "The owner says:" is a fact the owner states about this project, such as a situation that cannot happen ("tools are only lent to registered members"). Return it as an invariant over these attributes when they can express it, as for a definition; "term" is "owner".

Return only what a glossary entry or the owner states. Nothing a requirement wants, nothing you infer from the rules or the situation: a wrong fact invents or hides clashes. Most entries define nothing these attributes need; return no facts then.`

const defineSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["facts"],
  "properties": {
    "facts": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["term", "on", "when", "assert"],
        "properties": {"term": {"type": "string"}, "on": {"type": "string"}, "when": {"type": "string"}, "assert": {"type": "string"}}
      }
    }
  }
}`

// factSource says where a fact the definitions step read comes from.
func factSource(term string) string {
	if term == "owner" {
		return "the owner says: "
	}
	return "the glossary defines " + term + ": "
}
