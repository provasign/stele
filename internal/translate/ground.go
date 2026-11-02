package translate

import (
	"sort"
	"strings"
	"unicode"

	"github.com/provasign/stele/internal/ir"
)

// Related returns up to k vocabulary symbols whose names share words with
// the prose, best match first (docs/07 translator step 2, "ground"). It is a
// deterministic hint, shown beside each statement so the model reuses an
// existing symbol instead of inventing a synonym.
func Related(reg *ir.Registry, prose string, k int) []string {
	want := map[string]bool{}
	for _, w := range words(prose) {
		want[w] = true
	}
	type hit struct {
		name  string
		score int
	}
	var hits []hit
	consider := func(name string, parts ...string) {
		score := 0
		for _, p := range parts {
			for _, w := range words(p) {
				if want[w] {
					score++
				}
			}
		}
		if score > 0 {
			hits = append(hits, hit{name, score})
		}
	}
	for _, e := range reg.Entities {
		for _, a := range e.Attrs {
			consider(a.QName(), e.Name, a.Name, strings.Join(a.Values, " "))
		}
	}
	for _, op := range reg.Operations {
		_, verb, _ := strings.Cut(op.Name, ".")
		consider(op.Name, op.Entity.Name, verb)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].name < hits[j].name
	})
	var out []string
	for i := 0; i < len(hits) && i < k; i++ {
		out = append(out, hits[i].name)
	}
	return out
}

// words splits text and camelCase names into lower-case stems longer than
// two letters, skipping words too common to ground anything.
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 2 {
			w := stem(string(cur))
			if !common[w] {
				out = append(out, w)
			}
		}
		cur = cur[:0]
	}
	prev := rune(0)
	for _, r := range s {
		switch {
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			cur = append(cur, unicode.ToLower(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			cur = append(cur, unicode.ToLower(r))
		default:
			flush()
		}
		prev = r
	}
	flush()
	return out
}

// stem drops common English suffixes so warning, warnings and warn meet.
func stem(w string) string {
	for _, suf := range []string{"ing", "ed", "es", "s"} {
		if len(w) > len(suf)+3 && strings.HasSuffix(w, suf) {
			return strings.TrimSuffix(w, suf)
		}
	}
	return w
}

var common = map[string]bool{
	"the": true, "and": true, "must": true, "system": true, "not": true, "for": true, "with": true,
	"when": true, "that": true, "this": true, "are": true, "from": true, "into": true, "all": true,
	"any": true, "can": true, "user": false, "shall": true, "should": true, "allow": true, "provide": true,
	"support": true, "each": true, "their": true, "its": true, "have": true, "will": true, "use": true,
}

// Words splits text and camelCase names into the lower-case stems Related
// matches on.
func Words(s string) []string { return words(s) }
