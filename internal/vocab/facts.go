package vocab

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// Fact is one thing the specs state requirements about, found across all
// statements before anything is named per statement. Each fact becomes one
// attribute, so every requirement about it uses the same symbol.
type Fact struct {
	Name         string   `json:"name"` // Entity.attribute
	Meaning      string   `json:"meaning"`
	Type         string   `json:"type"`
	Values       []string `json:"values"`
	Min          int      `json:"min"`
	Max          int      `json:"max"`
	SetBy        string   `json:"set_by"` // system | world
	StatementIDs []string `json:"statement_ids"`
}

// Merge records a fact folded into another by consolidation.
type Merge struct {
	From   string `json:"from"`
	Into   string `json:"into"`
	Reason string `json:"reason"`
}

// findFacts lists the facts across all statements, chunk by chunk, each
// chunk seeing the facts found so far, then consolidates facts that only
// one statement needs.
func findFacts(ctx context.Context, m llm.Client, stmts []translate.Statement, base *ir.Registry, opt Options, rep *Report) ([]Fact, error) {
	var facts []Fact
	for i := 0; i < len(stmts); i += opt.FactBatch {
		chunk := stmts[i:min(i+opt.FactBatch, len(stmts))]
		opt.logf("  facts: statements %d-%d (%d facts so far)", i+1, i+len(chunk), len(facts))
		found, err := factChunk(ctx, m, chunk, base, facts, opt, rep)
		if err != nil {
			return nil, fmt.Errorf("facts for %s..%s: %w", chunk[0].ID, chunk[len(chunk)-1].ID, err)
		}
		facts = addFacts(facts, found)
	}
	merges, err := consolidate(ctx, m, facts, base, rep)
	if err != nil {
		// Consolidation only improves the facts; keep them unconsolidated.
		rep.Errors = append(rep.Errors, "consolidation: "+err.Error())
		return facts, nil
	}
	facts, rep.Merges = applyMerges(facts, merges)
	return facts, nil
}

func factChunk(ctx context.Context, m llm.Client, chunk []translate.Statement, base *ir.Registry, known []Fact, opt Options, rep *Report) ([]Fact, error) {
	system := factsPrompt + translate.RenderVocabulary(base) + "\n# Facts found so far\n\n" + renderFacts(known)
	var req strings.Builder
	req.WriteString("List the facts these statements state requirements about. Reuse a fact above, by its name, when a statement is about it.\n\n")
	for _, s := range chunk {
		fmt.Fprintf(&req, "statement_id: %s\ntext: %s\n\n", s.ID, s.Prose)
	}
	turns := []llm.Turn{{Role: "user", Text: req.String()}}
	for round := 0; ; round++ {
		reply, err := m.Complete(ctx, llm.Request{System: system, Turns: turns, Schema: factsSchema})
		if err != nil {
			return nil, err
		}
		rep.Rounds++
		rep.Usage.Add(reply.Usage)
		var out struct {
			Facts []Fact `json:"facts"`
		}
		if err := json.Unmarshal([]byte(reply.JSON), &out); err != nil {
			return nil, fmt.Errorf("reply is not the expected JSON: %w", err)
		}
		all := addFacts(known, out.Facts)
		_, lerr := loadWith(opt, factsProposal(base, all))
		if lerr == nil {
			return out.Facts, nil
		}
		if round >= opt.MaxRepairs {
			return nil, lerr
		}
		turns = append(turns, llm.Turn{Role: "assistant", Text: reply.JSON}, llm.Turn{Role: "user", Text: "These facts do not compile as vocabulary:\n" + lerr.Error() +
			"\n\nReturn the complete corrected list. A fact reused by name keeps its type and values."})
	}
}

// addFacts adds found to facts: a fact with a known name adds its
// statements to it (keeping the first type), a new one is appended.
func addFacts(facts, found []Fact) []Fact {
	out := append([]Fact{}, facts...)
	at := map[string]int{}
	for i, f := range out {
		at[f.Name] = i
	}
	for _, f := range found {
		if i, ok := at[f.Name]; ok {
			out[i].StatementIDs = union(out[i].StatementIDs, f.StatementIDs)
			continue
		}
		at[f.Name] = len(out)
		out = append(out, f)
	}
	return out
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// consolidate asks, once, which facts are another fact in other words. Facts
// found from different specs are the ones most likely to be split, since
// each chunk names what it reads.
func consolidate(ctx context.Context, m llm.Client, facts []Fact, base *ir.Registry, rep *Report) ([]Merge, error) {
	if len(facts) < 2 {
		return nil, nil
	}
	var req strings.Builder
	req.WriteString("Find the facts above that are the same thing under different names, often from different specs: one entity's flag and another's enum (Payment.captured and Order.paymentState both say whether the order's payment was taken), a fact and its negation (sessionExpired, sessionActive), or a requirement restated as a flag. Requirements can only contradict each other on a shared fact, so a split fact hides contradictions.\n\n")
	req.WriteString("For each, merge the narrower or less descriptive fact into the other; the statements of both then use the target. Merge only facts that say the same thing about the same object; a fact that is genuinely its own stays. Return only the merges.\n")
	reply, err := m.Complete(ctx, llm.Request{
		System: consolidatePrompt + translate.RenderVocabulary(base) + "\n# Facts\n\n" + renderFacts(facts),
		Turns:  []llm.Turn{{Role: "user", Text: req.String()}},
		Schema: mergeSchema,
	})
	if err != nil {
		return nil, err
	}
	rep.Rounds++
	rep.Usage.Add(reply.Usage)
	var out struct {
		Merges []Merge `json:"merges"`
	}
	if err := json.Unmarshal([]byte(reply.JSON), &out); err != nil {
		return nil, fmt.Errorf("reply is not the expected JSON: %w", err)
	}
	return out.Merges, nil
}

// applyMerges folds each valid merge's statements into its target. A merge
// is valid when both facts exist and differ; others are ignored, so the
// model can only move statements between facts, never invent or lose them.
func applyMerges(facts []Fact, merges []Merge) ([]Fact, []Merge) {
	at := map[string]int{}
	for i, f := range facts {
		at[f.Name] = i
	}
	dropped := map[string]string{} // merged fact -> its final target
	var applied []Merge
	for _, mg := range merges {
		into := mg.Into
		for dropped[into] != "" {
			into = dropped[into]
		}
		i, ok1 := at[mg.From]
		j, ok2 := at[into]
		if !ok1 || !ok2 || i == j || dropped[mg.From] != "" {
			continue
		}
		facts[j].StatementIDs = union(facts[j].StatementIDs, facts[i].StatementIDs)
		dropped[mg.From] = into
		applied = append(applied, Merge{From: mg.From, Into: into, Reason: mg.Reason})
	}
	var out []Fact
	for _, f := range facts {
		if dropped[f.Name] == "" {
			out = append(out, f)
		}
	}
	return out, applied
}

// factsProposal turns facts into entities and extensions.
func factsProposal(base *ir.Registry, facts []Fact) Proposal {
	byEntity := map[string][]Fact{}
	for _, f := range facts {
		ent, _, _ := strings.Cut(f.Name, ".")
		byEntity[ent] = append(byEntity[ent], f)
	}
	names := make([]string, 0, len(byEntity))
	for n := range byEntity {
		names = append(names, n)
	}
	sort.Strings(names)
	var p Proposal
	for _, n := range names {
		e := Entity{Name: n}
		for _, f := range byEntity[n] {
			_, attr, _ := strings.Cut(f.Name, ".")
			e.Attributes = append(e.Attributes, Attribute{Name: attr, Type: f.Type, Values: f.Values, Min: f.Min, Max: f.Max, Meaning: f.Meaning, SetBy: worldOnly(f.SetBy)})
			e.StatementIDs = union(e.StatementIDs, f.StatementIDs)
		}
		if base.Entities[n] != nil {
			p.Extensions = append(p.Extensions, e)
		} else {
			p.Entities = append(p.Entities, e)
		}
	}
	return p
}

func renderFacts(facts []Fact) string {
	if len(facts) == 0 {
		return "(none yet)\n"
	}
	var b strings.Builder
	for _, f := range facts {
		t := f.Type
		switch f.Type {
		case "enum":
			t = "enum {" + strings.Join(f.Values, ", ") + "}"
		case "int":
			t = fmt.Sprintf("int [%d, %d]", f.Min, f.Max)
		}
		if f.SetBy == "world" {
			t += ", set by the world"
		}
		fmt.Fprintf(&b, "  %s: %s - %s (%d statement(s))\n", f.Name, t, f.Meaning, len(f.StatementIDs))
	}
	return b.String()
}

// worldOnly keeps "world" and drops "system": an attribute is the system's
// when some operation writes it, which the vocabulary already says.
func worldOnly(setBy string) string {
	if setBy == "world" {
		return "world"
	}
	return ""
}

// reconsolidate merges attributes of the proposal that name one fact twice.
// Nothing has been translated yet, so no rule refers to either.
func reconsolidate(ctx context.Context, m llm.Client, base *ir.Registry, p Proposal, rep *Report) (Proposal, []Merge, error) {
	var facts []Fact
	for _, e := range append(append([]Entity{}, p.Entities...), p.Extensions...) {
		for _, a := range e.Attributes {
			facts = append(facts, Fact{Name: e.Name + "." + a.Name, Meaning: a.Meaning, Type: a.Type, Values: a.Values, Min: a.Min, Max: a.Max, SetBy: a.SetBy, StatementIDs: e.StatementIDs})
		}
	}
	merges, err := consolidate(ctx, m, facts, base, rep)
	if err != nil {
		return p, nil, err
	}
	out, applied := ApplyMerges(p, merges)
	return out, applied, nil
}

// ApplyMerges drops each merge's source attribute from the proposal and
// points every write of it at the target. A merge applies when its source is
// an attribute of the proposal and differs from its target; chains are
// followed. It returns the merges applied, with their final targets.
func ApplyMerges(p Proposal, merges []Merge) (Proposal, []Merge) {
	have := map[string]bool{}
	for _, e := range append(append([]Entity{}, p.Entities...), p.Extensions...) {
		for _, a := range e.Attributes {
			have[e.Name+"."+a.Name] = true
		}
	}
	into := map[string]string{}
	var applied []Merge
	for _, mg := range merges {
		t := mg.Into
		for into[t] != "" {
			t = into[t]
		}
		if !have[mg.From] || into[mg.From] != "" || t == mg.From || !strings.Contains(t, ".") {
			continue
		}
		into[mg.From] = t
		applied = append(applied, Merge{From: mg.From, Into: t, Reason: mg.Reason})
	}
	for k, v := range into { // a target merged away later
		for into[v] != "" {
			v = into[v]
		}
		into[k] = v
	}
	for i := range applied {
		applied[i].Into = into[applied[i].From]
	}
	if len(applied) == 0 {
		return p, nil
	}
	drop := func(es []Entity) []Entity {
		var out []Entity
		for _, e := range es {
			var as []Attribute
			for _, a := range e.Attributes {
				if into[e.Name+"."+a.Name] == "" {
					as = append(as, a)
				}
			}
			e.Attributes = as
			out = append(out, e)
		}
		return out
	}
	out := Proposal{Entities: drop(p.Entities), Extensions: drop(p.Extensions)}
	for _, o := range p.Operations {
		var ws []string
		for _, w := range o.Writes {
			if t := into[w]; t != "" {
				w = t
			}
			if !slices.Contains(ws, w) {
				ws = append(ws, w)
			}
		}
		o.Writes = ws
		out.Operations = append(out.Operations, o)
	}
	return out, applied
}

// Used is an attribute the translated rules read, with statements that use it.
type Used struct {
	Name     string // Entity.attribute
	Type     string // as the vocabulary renders it
	Meaning  string
	Examples []string // statement texts, shortened
	Text     string   // every statement text using it, for similarity
}

// ConsolidateUsed asks which of the attributes the rules use are one fact
// under two names. Only the merges are returned; the caller applies them and
// translates the affected statements again.
func ConsolidateUsed(ctx context.Context, m llm.Client, base *ir.Registry, used []Used, rep *Report) ([]Merge, error) {
	if len(used) < 2 {
		return nil, nil
	}
	var list strings.Builder
	for _, u := range used {
		fmt.Fprintf(&list, "%s: %s", u.Name, u.Type)
		if u.Meaning != "" {
			fmt.Fprintf(&list, " - %s", u.Meaning)
		}
		list.WriteString("\n")
		for _, x := range u.Examples {
			fmt.Fprintf(&list, "    used by: %s\n", x)
		}
	}
	req := "Below are the attributes the translated requirements use, each with requirements that use it. Find attributes that are the same fact under different names, on the same entity or on different ones: a flag on one entity and an enum value on another that say the same thing, a fact and its negation, a requirement restated as a flag beside the state it constrains, the same setting seen from two components. Requirements can only contradict each other on a shared fact, so every split fact hides contradictions.\n\n" +
		"For each, merge the narrower or less descriptive attribute into the other. Merge only when every requirement using the source can be stated with the target; attributes that are related but different facts stay apart. Return only the merges.\n\n" + list.String()
	reply, err := m.Complete(ctx, llm.Request{
		System: consolidatePrompt + translate.RenderVocabulary(base),
		Turns:  []llm.Turn{{Role: "user", Text: req}},
		Schema: mergeSchema,
	})
	if err != nil {
		return nil, err
	}
	rep.Rounds++
	rep.Usage.Add(reply.Usage)
	var out struct {
		Merges []Merge `json:"merges"`
	}
	if err := json.Unmarshal([]byte(reply.JSON), &out); err != nil {
		return nil, fmt.Errorf("reply is not the expected JSON: %w", err)
	}
	return out.Merges, nil
}
