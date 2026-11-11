package formalize

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// onePass asks for vocabulary and rules together: the model sees the rule
// schema, the vocabulary format and a chunk of statements, and returns both
// at once, so it names each fact as it writes the rules that use it. Chunks
// run in order, each seeing the vocabulary so far. Compile errors go back for
// repair, like the other steps.
func onePass(ctx context.Context, m llm.Client, stmts []translate.Statement, st *state, opt Options, dir string) error {
	chunk := opt.OnePassChunk
	if chunk <= 0 {
		chunk = 100
	}
	repairs := opt.MaxRepairs
	if repairs == 0 {
		repairs = 2
	}
	schema, err := combinedSchema()
	if err != nil {
		return err
	}
	vopt := vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}
	// Validate tentative vocabulary outside the registry; rejected proposals
	// must not replace the last accepted vocabulary.
	validationDir, err := os.MkdirTemp(dir, ".onepass-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(validationDir)
	for i := st.OnePassNext; i < len(stmts); i += chunk {
		part := stmts[i:min(i+chunk, len(stmts))]
		fmt.Fprintf(opt.Log, "  one pass: statements %d-%d\n", i+1, i+len(part))
		cur, err := vocab.Load(vopt, st.Proposal)
		if err != nil {
			return err
		}
		system := onePassPrompt + translate.RenderVocabulary(cur)
		var req strings.Builder
		req.WriteString("Build the vocabulary these statements need, beyond the vocabulary above, and translate every statement into rules over it. Reuse existing symbols whenever they mean the same thing.\n\n")
		for _, s := range part {
			fmt.Fprintf(&req, "statement_id: %s\nsource: %s\ntext: %s\n\n", s.ID, llm.Data(s.Source), llm.Data(s.Prose))
			for _, note := range s.Notes {
				fmt.Fprintf(&req, "vocabulary note: %s\n", note)
			}
		}
		turns := []llm.Turn{{Role: "user", Text: req.String()}}
		for round := 0; ; round++ {
			reply, err := m.Complete(ctx, llm.Request{System: system, Turns: turns, Schema: schema})
			if err != nil {
				return fmt.Errorf("one pass %d-%d: %w", i+1, i+len(part), err)
			}
			st.Usage.Add(reply.Usage)
			// Preserve usage even when this reply needs repair or the next
			// request fails. Only accepted chunks advance OnePassNext.
			if err := saveState(dir, st); err != nil {
				return err
			}
			var out struct {
				vocab.Proposal
				Rules []translate.Candidate `json:"rules"`
			}
			var problems []string
			var p vocab.Proposal
			if err := json.Unmarshal([]byte(reply.JSON), &out); err != nil {
				problems = append(problems, "reply is not the expected JSON: "+err.Error())
			} else {
				problems = replyCoverage(out.Rules, part)
				p = vocab.Merged(st.Proposal, out.Proposal)
				if _, err := vocab.Load(vopt, p); err != nil {
					problems = append(problems, "the vocabulary does not compile: "+err.Error())
				} else {
					files, err := writeVocabulary(validationDir, p, opt)
					if err != nil {
						return err
					}
					assign(out.Rules)
					translate.Revalidate(files, out.Rules)
					for _, c := range out.Rules {
						if c.Error != "" {
							problems = append(problems, fmt.Sprintf("%s (statement %s): %s", c.RuleID, c.StatementID, c.Error))
						}
					}
				}
			}
			if len(problems) == 0 {
				st.Proposal = p
				st.Cands = append(st.Cands, out.Rules...)
				st.OnePassNext = i + len(part)
				if err := saveState(dir, st); err != nil {
					return err
				}
				break
			}
			if round >= repairs {
				return fmt.Errorf("one pass %d-%d: reply still invalid after %d repair(s): %s", i+1, i+len(part), round, strings.Join(problems, "; "))
			}
			turns = append(turns, llm.Turn{Role: "assistant", Text: reply.JSON}, llm.Turn{Role: "user", Text: "Some of the reply is missing or invalid:\n- " + strings.Join(problems, "\n- ") +
				"\n\nReturn the complete corrected reply: all the vocabulary and all the rules for these statements. Fix only what the errors point at."})
		}
	}
	return nil
}

// replyCoverage requires an outcome for every requested statement and rejects
// invented IDs, including IDs belonging to a different chunk.
func replyCoverage(cands []translate.Candidate, stmts []translate.Statement) []string {
	known, got := map[string]bool{}, map[string]bool{}
	for _, s := range stmts {
		known[s.ID] = true
	}
	var problems []string
	for _, c := range cands {
		got[c.StatementID] = true
		if !known[c.StatementID] {
			problems = append(problems, fmt.Sprintf("unknown statement_id %q", c.StatementID))
		}
		switch c.Outcome {
		case "rule", "informal", "needs_vocabulary":
		default:
			problems = append(problems, fmt.Sprintf("statement %s has invalid outcome %q", c.StatementID, c.Outcome))
		}
	}
	for _, s := range stmts {
		if !got[s.ID] {
			problems = append(problems, fmt.Sprintf("statement %s has no entry", s.ID))
		}
	}
	return problems
}

// assign gives rule IDs: the statement ID, then .2, .3 for further rules.
func assign(cands []translate.Candidate) {
	n := map[string]int{}
	for i := range cands {
		id := cands[i].StatementID
		n[id]++
		cands[i].RuleID = id
		if n[id] > 1 {
			cands[i].RuleID = fmt.Sprintf("%s.%d", id, n[id])
		}
	}
}

// combinedSchema is the proposer's schema with the translator's rules added.
func combinedSchema() (string, error) {
	var v, t map[string]any
	if err := json.Unmarshal([]byte(vocab.OutputSchema), &v); err != nil {
		return "", err
	}
	if err := json.Unmarshal([]byte(translate.OutputSchema), &t); err != nil {
		return "", err
	}
	v["properties"].(map[string]any)["rules"] = t["properties"].(map[string]any)["rules"]
	v["required"] = append(v["required"].([]any), "rules")
	js, err := json.Marshal(v)
	return string(js), err
}

var onePassPrompt = "You build a Stele registry from software requirements: first the vocabulary they talk about, then typed rules over it. Two requirements can be compared only if their rules use the same symbols for the same facts, so name each fact once and use that name in every rule about it, whichever statement it comes from.\n\n" +
	"# Part 1: vocabulary\n\n" + section(vocab.SystemPrompt, "# What vocabulary is", "# Current vocabulary") +
	"\n# Part 2: rules\n\n" + strings.Replace(section(translate.SystemPrompt, "# State and steps", "# Vocabulary"),
	"- Use only the declared vocabulary. If part of a statement needs a concept the vocabulary lacks, give that part outcome \"needs_vocabulary\" and list the missing terms.",
	"- Use only the vocabulary above and the vocabulary you propose in this reply. Propose what a statement needs rather than giving up on it; use \"needs_vocabulary\" only for a concept the rule language cannot express.", 1) +
	"\n# Vocabulary so far\n\n"

// section returns the text of prompt from the heading start up to the
// heading end.
func section(prompt, start, end string) string {
	i := strings.Index(prompt, start)
	j := strings.LastIndex(prompt, end)
	if i < 0 || j < i {
		return prompt
	}
	return prompt[i:j]
}
