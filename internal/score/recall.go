package score

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/review"
	"github.com/provasign/stele/internal/translate"
)

// Known is a contradiction established by a case study, named by the source
// lines of its statements so it can be matched however the text is segmented.
type Known struct {
	ID      string   `yaml:"id"`
	Sources []string `yaml:"sources"` // e.g. specs/014-shared-hosts/spec.md#L143
	// AnyOf lists alternative source sets, for a conflict that can be stated
	// through different statements of the same rules; any one set suffices.
	AnyOf      [][]string `yaml:"any_of"`
	WorldFacts []string   `yaml:"world_facts"` // e.g. AX1; informative, not used for matching
	Note       string     `yaml:"note"`
}

// LoadKnown reads a known-conflicts file.
func LoadKnown(path string) ([]Known, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Known
	if err := yaml.Unmarshal(src, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

// Finding is one reported contradiction, from either system, as source lines.
type Finding struct {
	Sources []string `json:"sources"`
	Detail  string   `json:"detail"`
}

// KnownResult says whether, and by which finding, a known conflict was found.
type KnownResult struct {
	ID          string `json:"id"`
	Found       bool   `json:"found"`
	By          int    `json:"by_finding,omitempty"` // 1-based index into findings
	Unsegmented bool   `json:"unsegmented,omitempty"`
}

// RecallResult scores one system's findings against the known conflicts.
type RecallResult struct {
	System   string        `json:"system"`
	Known    []KnownResult `json:"known"`
	Found    int           `json:"found"`
	Findings []Finding     `json:"findings"`
	Matched  int           `json:"findings_matching_a_known_conflict"`
}

// Recall marks a known conflict found when some finding cites every one of its
// source lines. The same rule applies to the translator and the reviewer. A
// finding may cite more lines than the known conflict: it still names the
// contradiction, and its extra lines are visible for review.
func Recall(system string, known []Known, findings []Finding, stmts []translate.Statement) *RecallResult {
	inCorpus := map[string]bool{}
	for _, s := range stmts {
		inCorpus[s.Source] = true
	}
	res := &RecallResult{System: system, Findings: findings}
	matched := map[int]bool{}
	for _, k := range known {
		kr := KnownResult{ID: k.ID}
		sets := k.AnyOf
		if len(k.Sources) > 0 {
			sets = append([][]string{k.Sources}, sets...)
		}
		segmented := false
		for _, set := range sets {
			ok := true
			for _, src := range set {
				ok = ok && inCorpus[src]
			}
			segmented = segmented || ok
		}
		kr.Unsegmented = !segmented
	find:
		for i, f := range findings {
			for _, set := range sets {
				if covers(f.Sources, set) {
					kr.Found, kr.By = true, i+1
					matched[i] = true
					break find
				}
			}
		}
		if kr.Found {
			res.Found++
		}
		res.Known = append(res.Known, kr)
	}
	res.Matched = len(matched)
	return res
}

func covers(have, want []string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// sourcesOf maps statement IDs to their sources; world facts and unknown IDs
// keep their names.
func sourcesOf(ids []string, stmts map[string]translate.Statement) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		src := id
		if s, ok := stmts[id]; ok {
			src = s.Source
		}
		if !seen[src] {
			seen[src] = true
			out = append(out, src)
		}
	}
	sort.Strings(out)
	return out
}

func index(stmts []translate.Statement) map[string]translate.Statement {
	m := map[string]translate.Statement{}
	for _, s := range stmts {
		m[s.ID] = s
	}
	return m
}

// CheckerFindings turns a check of a translated registry into findings.
func CheckerFindings(res *checker.Result, stmts []translate.Statement, regs ...*ir.Registry) []Finding {
	idx := index(stmts)
	var out []Finding
	add := func(rules []string, detail string) {
		ids := make([]string, len(rules))
		for i, r := range rules {
			ids[i] = statementFor(r, idx, regs...)
		}
		out = append(out, Finding{Sources: sourcesOf(ids, idx), Detail: detail})
	}
	for _, c := range res.Conflicts {
		parts := make([]string, len(c.Counterexample))
		for i, b := range c.Counterexample {
			parts[i] = b.Name + "=" + b.Value
		}
		add(c.Rules, "conflict: "+strings.Join(c.Rules, " + ")+" at "+strings.Join(parts, ", "))
	}
	for _, v := range res.Vacuous {
		add(append([]string{v.Rule}, v.Because...), "vacuous: "+v.Rule+" never applies, because of "+strings.Join(v.Because, " + "))
	}
	for _, d := range res.Dead {
		if d.NeverAllowed { // a vocabulary choice that cites no statement
			continue
		}
		add(d.Rules, "dead response "+d.Response+": "+strings.Join(d.Rules, " + "))
	}
	return out
}

// ReviewFindings turns a review into findings.
func ReviewFindings(r *review.Result, stmts []translate.Statement) []Finding {
	idx := index(stmts)
	var out []Finding
	for _, f := range r.Findings {
		ids := append(append([]string{}, f.StatementIDs...), f.WorldFacts...)
		out = append(out, Finding{Sources: sourcesOf(ids, idx), Detail: strings.Join(f.StatementIDs, " + ") + ": " + f.Situation})
	}
	return out
}

// StatementFor maps a rule ID to its current statement. When a registry is
// supplied, explicit source.statementId takes precedence over the rule ID's
// original prefix. Legacy rules fall back to that prefix.
func StatementFor(rule string, stmts []translate.Statement, regs ...*ir.Registry) string {
	return statementFor(rule, index(stmts), regs...)
}

// statementFor maps a rule ID to its statement: the rule ID itself, or the
// longest prefix before a "." that is a statement ID (003/FR-025.stopped,
// 013/FR-014.2). Statement IDs may contain dots themselves (001/research.md:L639).
func statementFor(rule string, idx map[string]translate.Statement, regs ...*ir.Registry) string {
	if len(regs) > 0 && regs[0] != nil {
		if x := regs[0].Rule(rule); x != nil && x.StatementID != "" {
			return x.StatementID
		}
	}
	for id := rule; ; {
		if _, ok := idx[id]; ok {
			return id
		}
		i := strings.LastIndexByte(id, '.')
		if i <= 0 {
			return translate.StatementOf(rule)
		}
		id = id[:i]
	}
}
