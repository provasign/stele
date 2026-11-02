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
			loc := winsOver.FindStringIndex(clause)
			if loc == nil {
				continue
			}
			for label, ids := range labels {
				if len(ids) != 1 || ids[0] == id {
					continue
				}
				if regexp.MustCompile(`(^|[^\w-])`+regexp.QuoteMeta(label)+`($|[^\w-])`).MatchString(clause[loc[1]:]) && !slices.Contains(over[id], ids[0]) {
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
