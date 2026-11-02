// Package review is the LLM-only baseline of stele-research docs/design-archive/12-benchmarking.md: a model
// reads the statements, with the same world facts the translator gets, and
// lists the ones that cannot all hold. No IR and no solver are involved.
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

const systemPrompt = `You review software requirements for contradictions: sets of requirements that cannot all hold at once in some situation the system can face. Requirements written months apart, in different specs, are the usual source.

For each contradiction, name every statement involved by its statement_id, name any of the given world facts it relies on, and explain in one or two sentences the situation in which they cannot all hold. Report a contradiction only if you can name that situation. Do not report requirements that are merely related, redundant or vague, and leave out any pair you find compatible on reflection: never report a finding whose situation says there is no conflict.

Be thorough: check every statement against every other, across all specs, not only neighbours.`

const outputSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["conflicts"],
  "properties": {
    "conflicts": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["statement_ids", "world_facts", "situation"],
        "properties": {
          "statement_ids": {"type": "array", "items": {"type": "string"}},
          "world_facts": {"type": "array", "items": {"type": "string"}},
          "situation": {"type": "string"}
        }
      }
    }
  }
}`

// Finding is one contradiction the reviewer reports.
type Finding struct {
	StatementIDs []string `json:"statement_ids"`
	WorldFacts   []string `json:"world_facts"`
	Situation    string   `json:"situation"`
	Batch        string   `json:"batch,omitempty"` // which kind of review call found it, when the corpus was split
}

// Result is a review run.
type Result struct {
	Backend  string    `json:"backend"`
	Model    string    `json:"model,omitempty"`
	Findings []Finding `json:"conflicts"`
	Unknown  []string  `json:"unknown_statement_ids,omitempty"` // IDs the reviewer cited that do not exist
	Usage    llm.Usage `json:"usage"`
	Batches  int       `json:"batches,omitempty"` // review calls, when the corpus was split
	Failed   []string  `json:"failed_batches,omitempty"`
}

// Fact is a world fact given to the reviewer as prose.
type Fact struct{ ID, Text string }

// Review asks m for every contradiction among stmts.
func Review(ctx context.Context, m llm.Client, stmts []translate.Statement, facts []Fact) (*Result, error) {
	var b strings.Builder
	if len(facts) > 0 {
		b.WriteString("World facts about the system:\n")
		for _, f := range facts {
			fmt.Fprintf(&b, "- %s: %s\n", f.ID, f.Text)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Requirements (%d statements):\n\n", len(stmts))
	known := map[string]bool{}
	for _, s := range stmts {
		known[s.ID] = true
		fmt.Fprintf(&b, "statement_id: %s\nsource: %s\ntext: %s\n\n", s.ID, s.Source, s.Prose)
	}
	b.WriteString("List every contradiction.")
	reply, err := m.Complete(ctx, llm.Request{System: systemPrompt, Schema: outputSchema,
		Turns: []llm.Turn{{Role: "user", Text: b.String()}}})
	if err != nil {
		return nil, err
	}
	res := &Result{Backend: m.Name(), Model: reply.Model, Usage: reply.Usage}
	if err := json.Unmarshal([]byte(reply.JSON), res); err != nil {
		return nil, fmt.Errorf("reply is not the expected JSON: %w\n%.2000s", err, reply.JSON)
	}
	if err := thin(res, Tokens(translate.Statement{Prose: b.String()}), reply.Usage); err != nil {
		return nil, err
	}
	kept := res.Findings[:0]
	for _, f := range res.Findings {
		if !finding(f) {
			continue
		}
		for _, id := range f.StatementIDs {
			if !known[id] {
				res.Unknown = append(res.Unknown, id)
			}
		}
		kept = append(kept, f)
	}
	res.Findings = kept
	return res, nil
}

var notAConflict = regexp.MustCompile(`(?i)\b(not|no) (a |an )?(true |real |actual |genuine )?(conflict|contradiction)s?\b|\bplaceholder\b`)

// finding reports whether f is a contradiction as reported: two distinct
// statements, or one against a world fact, in a situation that does not
// itself say there is none (ISSUES 48).
func finding(f Finding) bool {
	ids := map[string]bool{}
	for _, id := range f.StatementIDs {
		ids[id] = true
	}
	if len(ids) < 2 && (len(ids) == 0 || len(f.WorldFacts) == 0) {
		return false
	}
	return !notAConflict.MatchString(f.Situation)
}

// thin rejects a reply too small for its prompt: a large corpus answered
// with at most one finding in a few hundred output tokens, or with
// placeholder text, is a failed call, not a clean corpus (ISSUES 46).
func thin(res *Result, promptTokens int, u llm.Usage) error {
	if promptTokens < 50000 || len(res.Findings) > 1 {
		return nil
	}
	if u.OutputTokens > 0 && u.OutputTokens < 400 {
		return fmt.Errorf("a %d-token prompt got %d output tokens and %d finding(s): the reply is too thin to trust; split the corpus (--budget-tokens)", promptTokens, u.OutputTokens, len(res.Findings))
	}
	for _, f := range res.Findings {
		if notAConflict.MatchString(f.Situation) {
			return fmt.Errorf("the only finding of a %d-token prompt is %q: the reply is not a review; split the corpus (--budget-tokens)", promptTokens, f.Situation)
		}
	}
	return nil
}
