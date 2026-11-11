package formalize

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// Requests written in parallel each name the steps they need, so one act
// can get two names on two entities: "a suspended member MUST NOT borrow
// tools" was read as Member.borrow while the lending rules were read as
// Tool.lend, and the suspension rule could then clash with nothing; the
// check said the requirements agree (a trial, 2026-11-11). Requirements
// about one act meet only on one step. consolidateSteps asks which new
// steps are an act another step already names, and reads the statements
// that use the new name again, told to use the other.

const stepsPrompt = `You check the steps (operations) of a software system's vocabulary.
Translators working in parallel on different requirement statements named
the steps they needed, so one act can have two names on two entities: a
member borrowing a tool and the tool being lent are one act (Member.borrow
and Tool.lend); an order shipping and its shipment being sent are one act.
Requirements about one act can only contradict each other when they name
the same step, so a split step hides contradictions.

Each item gives a step, whether it is new, and statements whose rules are
about it. Name the new steps that are the same act as another step under a
different name: the same thing happening, at the same moment, on the same
request. Keep apart steps that are different acts even when related
(reserving a tool and lending it; creating an order and paying for it), and
a step and one that follows it. When unsure, keep them apart.

For each, merge the new step into the other one, with one sentence of
reason. Reply with no merges when every step is distinct.
`

// StepMerge records a step read again as another.
type StepMerge = vocab.Merge

func consolidateSteps(ctx context.Context, m llm.Client, stmts []translate.Statement, st *state, opt Options, dir string) error {
	reg, err := vocab.Load(vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}, st.Proposal)
	if err != nil {
		return fmt.Errorf("step consolidation: %w", err)
	}
	prose := map[string]string{}
	for _, s := range stmts {
		prose[s.ID] = s.Prose
	}
	ours := map[string]bool{}
	for _, op := range st.Proposal.Operations {
		ours[op.Name] = true
	}
	users := map[string][]string{} // step -> statements whose rules are about it
	for _, c := range st.Cands {
		if c.Outcome != "rule" || c.Error != "" {
			continue
		}
		for _, op := range []string{c.Operation, c.On} {
			if op != "" && reg.Operations[op] != nil && !slices.Contains(users[op], c.StatementID) {
				users[op] = append(users[op], c.StatementID)
			}
		}
	}
	var fresh []string
	for op := range users {
		if ours[op] {
			fresh = append(fresh, op)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	// The other steps: those rules use, and the vocabulary's own (an
	// approved step a change's new one may duplicate), up to a bound.
	var steps []string
	for name := range reg.Operations {
		if users[name] != nil || !ours[name] {
			steps = append(steps, name)
		}
	}
	sort.Strings(steps)
	if len(steps) > 120 {
		steps = slices.DeleteFunc(steps, func(s string) bool { return users[s] == nil })
	}
	if len(steps) < 2 {
		return nil
	}
	fmt.Fprintf(opt.Log, "consolidate steps: %d step(s), %d new ...\n", len(steps), len(fresh))
	var req strings.Builder
	for _, name := range steps {
		state := "approved"
		if ours[name] {
			state = "new"
		}
		fmt.Fprintf(&req, "step: %s (%s)\n", name, state)
		for k, id := range users[name] {
			if k == 2 {
				break
			}
			fmt.Fprintf(&req, "  used by: %s\n", llm.Data(oneLine(prose[id], 220)))
		}
	}
	reply, err := m.Complete(ctx, llm.Request{System: stepsPrompt, Turns: []llm.Turn{{Role: "user", Text: req.String()}}, Schema: stepsSchema})
	if err != nil {
		st.Errors = append(st.Errors, "step consolidation: "+err.Error())
		return nil
	}
	st.Usage.Add(reply.Usage)
	var out struct {
		Merges []vocab.Merge `json:"merges"`
	}
	if err := json.Unmarshal([]byte(reply.JSON), &out); err != nil {
		st.Errors = append(st.Errors, "step consolidation: reply is not the expected JSON: "+err.Error())
		return nil
	}
	notes := map[string]string{}
	var ids []string
	for _, mg := range out.Merges {
		if !ours[mg.From] && ours[mg.Into] {
			mg.From, mg.Into = mg.Into, mg.From
		}
		if mg.From == mg.Into || !ours[mg.From] || users[mg.From] == nil || reg.Operations[mg.Into] == nil {
			continue
		}
		ent, _, _ := strings.Cut(mg.Into, ".")
		for _, id := range users[mg.From] {
			note := fmt.Sprintf("%s and %s are one act under two names: use %s for it (%s). In its rules self is the %s; reach anything else through it or the request.", mg.From, mg.Into, mg.Into, strings.TrimSuffix(mg.Reason, "."), ent)
			if notes[id] != "" {
				note = notes[id] + " " + note
			} else {
				ids = append(ids, id)
			}
			notes[id] = note
		}
		st.StepMerges = append(st.StepMerges, mg)
		fmt.Fprintf(opt.Log, "  consolidate steps: %s is %s\n", mg.From, mg.Into)
	}
	if len(ids) == 0 {
		return nil
	}
	fmt.Fprintf(opt.Log, "consolidate steps: reading %d statement(s) again ...\n", len(ids))
	return retranslate(ctx, m, stmts, ids, notes, st, opt, dir, nil)
}

const stepsSchema = `{"type":"object","additionalProperties":false,"required":["merges"],"properties":{"merges":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["from","into","reason"],"properties":{"from":{"type":"string"},"into":{"type":"string"},"reason":{"type":"string"}}}}}}`
