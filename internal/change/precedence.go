package change

import (
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/translate"
)

// winsOver is a phrase saying the requirement holding it prevails over
// what it names next: "regardless of `--color` (FR-003b)", "overriding
// user colour preferences (FR-003b)".
var winsOver = regexp.MustCompile(`(?i)\b(regardless of|overrid(e|es|ing)|takes? precedence over|notwithstanding|even if|even when|despite|in spite of)\b`)

// denied ends the text before a precedence verb that denies it: "MUST NOT
// override FR-001" read as overriding it, and a conflict with FR-001 passed
// (a code review, 2026-11-04). "regardless of" and the like qualify the
// requirement, not a verb: "MUST NOT stop regardless of FR-001" still
// prevails over FR-001.
var denied = regexp.MustCompile(`(?i)(\bnot|\bnever|n't|\bcannot|\bno longer)\s+(be\s+|ever\s+)?$`)

// prevails returns where the first precedence phrase of clause ends that
// the clause does not deny, or -1.
func prevails(clause string) int {
	for _, loc := range winsOver.FindAllStringIndex(clause, -1) {
		phrase := strings.ToLower(clause[loc[0]:loc[1]])
		verb := strings.HasPrefix(phrase, "overrid") || strings.HasPrefix(phrase, "take")
		if verb && denied.MatchString(clause[:loc[0]]) {
			continue
		}
		return loc[1]
	}
	return -1
}

// textPrecedence adds to each rule the statements its requirement's own
// text says it prevails over, named by their label (FR-003b) within the
// same clause after such a phrase. The translator is asked for this too,
// but does not always give it, and the text is what the owner wrote.
func textPrecedence(cands []translate.Candidate, prose map[string]string) {
	labels := map[string][]string{} // label -> statement IDs
	for id := range prose {
		tail := id[strings.LastIndexByte(id, '/')+1:]
		if strings.ContainsAny(tail, "0123456789") {
			labels[tail] = append(labels[tail], id)
		}
	}
	over := map[string][]string{}
	for id := range prose {
		for _, clause := range strings.FieldsFunc(prose[id], func(r rune) bool { return r == ';' || r == '\n' }) {
			end := prevails(clause)
			if end < 0 {
				continue
			}
			for label, ids := range labels {
				if len(ids) != 1 || ids[0] == id {
					continue
				}
				if regexp.MustCompile(`(^|[^\w-])`+regexp.QuoteMeta(label)+`($|[^\w-])`).MatchString(clause[end:]) && !slices.Contains(over[id], ids[0]) {
					over[id] = append(over[id], ids[0])
				}
			}
		}
	}
	for i := range cands {
		c := &cands[i]
		if c.Outcome != "rule" {
			continue
		}
		for _, o := range over[c.StatementID] {
			if !slices.Contains(c.Overrides, o) {
				c.Overrides = append(c.Overrides, o)
			}
		}
		sort.Strings(c.Overrides)
	}
}

// answerPrecedence adds to each rule of a requirement the owner said
// prevails over another (a "prevails" answer to a conflict between two
// requirements of the change) that other requirement, while the winner's
// text is the one answered about. No new reading: the rules stand as read,
// and the checks drop the other's where both apply (D-052).
func answerPrecedence(cands []translate.Candidate, as []Answer, prose map[string]string) {
	over := map[string][]string{}
	for _, a := range as {
		if a.Kind != "conflict" || !strings.HasPrefix(a.Choice, "prevails") {
			continue
		}
		for id, h := range a.Statements {
			if p, ok := prose[id]; ok && translate.TextHash(p) == h {
				over[id] = append(over[id], a.Over...)
			}
		}
	}
	for i := range cands {
		c := &cands[i]
		if c.Outcome != "rule" {
			continue
		}
		for _, o := range over[c.StatementID] {
			if !slices.Contains(c.Overrides, o) {
				c.Overrides = append(c.Overrides, o)
			}
		}
		sort.Strings(c.Overrides)
	}
}
