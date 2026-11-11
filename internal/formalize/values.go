package formalize

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// Requests written in parallel each add the values they need to a shared
// enum, so one outcome can get two names ("error" in one batch, "rejected"
// in another), and rules that mean the same thing then demand different
// values. consolidateValues asks which values of each such enum name the
// same thing, keeps one, and rewrites the rules that read the enum to use
// it. A general and a more specific value stay apart.

const valuesPrompt = `You check the values of enum attributes in a software system's vocabulary.
The values were proposed by several translators working in parallel on
different requirement statements, so one thing can have two names. Each
item gives an attribute, its values and, for each value, statements whose
rules use it.

Group values only when every statement using them means the same thing by
them (one outcome, state or kind under two names). Keep apart values for
different things, and a general value and a more specific one (failed and
notFound; active and paused), even when they overlap. When unsure, keep
them apart.

Reply with one group per set of values that mean the same thing: the
attribute, the value to keep (the clearest name), the values to merge into
it, and one sentence of reason. Reply with no groups when every value is
distinct.
`

const valuesSchema = `{"type":"object","additionalProperties":false,"required":["groups"],"properties":{"groups":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["attribute","keep","merge","reason"],"properties":{"attribute":{"type":"string"},"keep":{"type":"string"},"merge":{"type":"array","items":{"type":"string"}},"reason":{"type":"string"}}}}}}`

// ValueMerge records enum values merged into one.
type ValueMerge struct {
	Attribute string   `json:"attribute"`
	Keep      string   `json:"keep"`
	Merged    []string `json:"merged"`
	Reason    string   `json:"reason"`
}

func consolidateValues(ctx context.Context, m llm.Client, stmts []translate.Statement, st *state, opt Options) error {
	reg, err := vocab.Load(vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}, st.Proposal)
	if err != nil {
		return fmt.Errorf("value consolidation: %w", err)
	}
	prose := map[string]string{}
	for _, s := range stmts {
		prose[s.ID] = s.Prose
	}
	ours := map[string]bool{}
	for _, e := range append(slices.Clone(st.Proposal.Entities), st.Proposal.Extensions...) {
		for _, a := range e.Attributes {
			if strings.EqualFold(strings.TrimSpace(a.Type), "enum") {
				ours[e.Name+"."+a.Name] = true
			}
		}
	}
	rules := parseRules(reg, st.Cands)
	// For each enum value: the statements whose rules read the attribute
	// and mention the value.
	users := map[string]map[string][]string{}
	for _, r := range rules {
		text := r.cand.When + " " + r.cand.Assert
		for _, a := range r.reads {
			q := a.QName()
			if !ours[q] || a.Kind != ir.Enum {
				continue
			}
			for _, v := range a.Values {
				if mentions(text, v) {
					if users[q] == nil {
						users[q] = map[string][]string{}
					}
					if !slices.Contains(users[q][v], r.cand.StatementID) {
						users[q][v] = append(users[q][v], r.cand.StatementID)
					}
				}
			}
		}
	}
	var items []string
	for q, byValue := range users {
		stmtsOf := map[string]bool{}
		for _, ids := range byValue {
			for _, id := range ids {
				stmtsOf[id] = true
			}
		}
		if len(byValue) < 2 || len(stmtsOf) < 2 {
			continue
		}
		items = append(items, q)
	}
	sort.Strings(items)
	if len(items) == 0 {
		return nil
	}
	fmt.Fprintf(opt.Log, "consolidate values: %d enum attribute(s) used by several statements ...\n", len(items))
	render := func(q string) string {
		var b strings.Builder
		ent, an, _ := strings.Cut(q, ".")
		a := reg.Entities[ent].Attrs[an]
		fmt.Fprintf(&b, "attribute: %s (values: %s)\n", q, strings.Join(a.Values, ", "))
		for _, v := range a.Values {
			ids := users[q][v]
			if len(ids) == 0 {
				fmt.Fprintf(&b, "  %s: used by no rule\n", v)
				continue
			}
			for k, id := range ids {
				if k == 2 {
					break
				}
				fmt.Fprintf(&b, "  %s: %s\n", v, llm.Data(oneLine(prose[id], 220)))
			}
		}
		return b.String()
	}
	const per = 25
	var parts [][]string
	for i := 0; i < len(items); i += per {
		parts = append(parts, items[i:min(i+per, len(items))])
	}
	got := make([][]ValueMerge, len(parts))
	errs := make([]error, len(parts))
	sem := make(chan struct{}, max(opt.Parallel, 1))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for p, part := range parts {
		wg.Add(1)
		go func(p int, part []string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var req strings.Builder
			for _, q := range part {
				req.WriteString(render(q))
				req.WriteString("\n")
			}
			reply, err := m.Complete(ctx, llm.Request{System: valuesPrompt, Turns: []llm.Turn{{Role: "user", Text: req.String()}}, Schema: valuesSchema})
			if err != nil {
				errs[p] = err
				return
			}
			mu.Lock()
			st.Usage.Add(reply.Usage)
			mu.Unlock()
			var out struct {
				Groups []struct {
					Attribute, Keep, Reason string
					Merge                   []string
				} `json:"groups"`
			}
			if err := json.Unmarshal([]byte(reply.JSON), &out); err != nil {
				errs[p] = err
				return
			}
			for _, g := range out.Groups {
				if slices.Contains(part, g.Attribute) {
					got[p] = append(got[p], ValueMerge{Attribute: g.Attribute, Keep: g.Keep, Merged: g.Merge, Reason: g.Reason})
				}
			}
		}(p, part)
	}
	wg.Wait()
	var merges []ValueMerge
	for p := range parts {
		if errs[p] != nil {
			st.Errors = append(st.Errors, "value consolidation: "+errs[p].Error())
			continue
		}
		merges = append(merges, got[p]...)
	}
	applied := applyValueMerges(reg, st, rules, merges)
	for _, mg := range applied {
		fmt.Fprintf(opt.Log, "  consolidate values: %s: %s merged into %s (%s)\n", mg.Attribute, strings.Join(mg.Merged, ", "), mg.Keep, mg.Reason)
	}
	st.ValueMerges = append(st.ValueMerges, applied...)
	fmt.Fprintf(opt.Log, "consolidate values: %d group(s) merged\n", len(applied))
	return nil
}

// applyValueMerges drops the merged values from each enum and rewrites the
// rules that read the attribute. A rule that also reads another enum with
// a merged value's name is left as it is (the name is ambiguous in it).
func applyValueMerges(reg *ir.Registry, st *state, rules []parsedRule, merges []ValueMerge) []ValueMerge {
	var applied []ValueMerge
	for _, mg := range merges {
		ent, an, _ := strings.Cut(mg.Attribute, ".")
		e := reg.Entities[ent]
		if e == nil || e.Attrs[an] == nil || e.Attrs[an].Kind != ir.Enum || !slices.Contains(e.Attrs[an].Values, mg.Keep) {
			continue
		}
		var merged []string
		for _, v := range mg.Merged {
			if v != mg.Keep && slices.Contains(e.Attrs[an].Values, v) {
				merged = append(merged, v)
			}
		}
		if len(merged) == 0 {
			continue
		}
		for _, r := range rules {
			reads := false
			clash := false
			for _, a := range r.reads {
				if a.QName() == mg.Attribute {
					reads = true
					continue
				}
				for _, v := range merged {
					if a.Kind == ir.Enum && slices.Contains(a.Values, v) || a.Elem != nil && a.Elem.Kind == ir.Enum && slices.Contains(a.Elem.Values, v) {
						clash = true
					}
				}
			}
			if !reads || clash {
				continue
			}
			for _, v := range merged {
				r.cand.When = renameValue(r.cand.When, v, mg.Keep)
				r.cand.Assert = renameValue(r.cand.Assert, v, mg.Keep)
			}
		}
		for _, es := range []*[]vocab.Entity{&st.Proposal.Entities, &st.Proposal.Extensions} {
			for i := range *es {
				if (*es)[i].Name != ent {
					continue
				}
				for k := range (*es)[i].Attributes {
					if a := &(*es)[i].Attributes[k]; a.Name == an {
						a.Values = slices.DeleteFunc(slices.Clone(a.Values), func(v string) bool { return slices.Contains(merged, v) })
					}
				}
			}
		}
		mg.Merged = merged
		applied = append(applied, mg)
	}
	return applied
}

var quoted = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

func mentions(expr, value string) bool {
	return renameValue(expr, value, "\x00") != expr
}

// renameValue replaces the bare word from with to outside string literals
// and dotted paths.
func renameValue(expr, from, to string) string {
	if expr == "" {
		return expr
	}
	word := regexp.MustCompile(`(^|[^\w.])` + regexp.QuoteMeta(from) + `($|[^\w.(])`)
	var b strings.Builder
	last := 0
	for _, loc := range quoted.FindAllStringIndex(expr, -1) {
		b.WriteString(replaceWord(word, expr[last:loc[0]], to))
		b.WriteString(expr[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(replaceWord(word, expr[last:], to))
	return b.String()
}

func replaceWord(word *regexp.Regexp, s, to string) string {
	// Matches can share a separator ("a,a"), so repeat until stable.
	for {
		next := word.ReplaceAllString(s, "${1}"+to+"${2}")
		if next == s {
			return s
		}
		s = next
	}
}
