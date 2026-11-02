package adopt

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/score"
	"github.com/provasign/stele/internal/translate"
)

// SyncReport compares the statements a registry was built from with a fresh
// import (docs/08 W5 step 9).
type SyncReport struct {
	Unchanged int
	// Added statements go through W1: translate them.
	Added []string
	// Changed statements keep their anchor but not their text: re-translate
	// them and bump each rule's version, which makes decisions pinning the
	// old version stale.
	Changed []string
	// Moved maps an old anchor to the new anchor of the same text. Their
	// rules keep their IDs.
	Moved map[string]string
	// Removed statements are gone; their live rules are retired, which
	// closes the open conflicts they were in.
	Removed []string
	// Retire lists the live rules of removed statements.
	Retire []string
	// RulesOf lists the live rules of changed statements, with their versions.
	RulesOf map[string][]string
	// Sources updates rule provenance independently of stable rule IDs.
	Sources map[string]RuleSource
	// Ambiguous moves have several possible identities; applying sync must
	// wait until the owner provides an unambiguous statement correspondence.
	Ambiguous map[string][]string
}

type RuleSource struct {
	StatementID string
	Doc         string
	Hash        string
}

// Sync matches statements by anchor, then by text hash for anchors that
// moved. A statement whose anchor and text both changed shows as one removed
// and one added; the owner says whether they are the same.
func Sync(old, cur []translate.Statement, reg *ir.Registry) *SyncReport {
	return SyncRenamed(old, cur, reg, nil)
}

// SyncRenamed is Sync told which old statement IDs became which new ones
// (a spec tool's RENAMED record, or the owner's answer). A renamed
// statement is the same statement: with the same body it is moved and its
// rules keep their IDs; with a new body it is changed as well, and its
// rules are listed for a new version under the new ID.
func SyncRenamed(old, cur []translate.Statement, reg *ir.Registry, renamed map[string]string) *SyncReport {
	rep := &SyncReport{Moved: map[string]string{}, RulesOf: map[string][]string{}, Sources: map[string]RuleSource{}, Ambiguous: map[string][]string{}}
	curByID := map[string]translate.Statement{}
	curByHash := map[string][]string{}
	for _, s := range cur {
		curByID[s.ID] = s
		h := translate.TextHash(s.Prose)
		curByHash[h] = append(curByHash[h], s.ID)
	}
	matchedOld, matchedCur := map[string]bool{}, map[string]bool{}
	changedOld := map[string]bool{} // renamed statements whose body changed too, by old ID
	for _, s := range old {
		to, ok := renamed[s.ID]
		c, found := curByID[to]
		if !ok || !found || matchedCur[to] {
			continue
		}
		matchedOld[s.ID], matchedCur[to] = true, true
		rep.Moved[s.ID] = to
		if Body(c.Prose) == Body(s.Prose) {
			rep.Unchanged++
		} else {
			changedOld[s.ID] = true
			rep.Changed = append(rep.Changed, to)
		}
	}
	unstable := map[string]bool{}
	for _, s := range append(append([]translate.Statement{}, old...), cur...) {
		if i := strings.LastIndex(s.ID, ":L"); i >= 0 {
			if _, err := strconv.Atoi(s.ID[i+2:]); err == nil {
				unstable[s.ID], unstable[s.ID[:i]] = true, true
			}
		}
		if i, h := strings.LastIndexByte(s.ID, '/'), strings.IndexByte(s.ID, '#'); h >= 0 && i > h {
			if _, err := strconv.Atoi(s.ID[i+1:]); err == nil {
				unstable[s.ID] = true // Markdown's section ordinal
			}
		}
	}
	// An explicit source ID identifies an edit even when someone copies its
	// previous text into a new statement. Only positional IDs can be reused
	// merely because lines or section ordinals shift.
	for _, s := range old {
		if c, ok := curByID[s.ID]; ok && !unstable[s.ID] {
			matchedOld[s.ID], matchedCur[s.ID] = true, true
			if translate.TextHash(c.Prose) == translate.TextHash(s.Prose) {
				rep.Unchanged++
			} else {
				rep.Changed = append(rep.Changed, s.ID)
			}
		}
	}
	matchOccurrences(old, cur, matchedOld, matchedCur, rep)
	oldByHash := map[string][]string{}
	// Preserve exact matches first. Then match text before treating a reused
	// line-based ID as an edit: an insertion can put another statement there.
	for _, s := range old {
		if matchedOld[s.ID] {
			continue
		}
		c, ok := curByID[s.ID]
		if ok && translate.TextHash(c.Prose) == translate.TextHash(s.Prose) {
			rep.Unchanged++
			matchedOld[s.ID], matchedCur[s.ID] = true, true
		} else {
			h := translate.TextHash(s.Prose)
			oldByHash[h] = append(oldByHash[h], s.ID)
		}
	}
	for _, s := range old {
		if matchedOld[s.ID] {
			continue
		}
		h := translate.TextHash(s.Prose)
		var targets []string
		for _, id := range curByHash[h] {
			if !matchedCur[id] {
				targets = append(targets, id)
			}
		}
		if len(targets) == 1 && len(oldByHash[h]) == 1 {
			rep.Moved[s.ID] = targets[0]
			matchedOld[s.ID], matchedCur[targets[0]] = true, true
		} else if len(targets) > 0 {
			rep.Ambiguous[s.ID] = targets
			matchedOld[s.ID] = true
		}
	}
	for _, s := range old {
		if matchedOld[s.ID] {
			continue
		}
		if _, ok := curByID[s.ID]; ok && !matchedCur[s.ID] {
			rep.Changed = append(rep.Changed, s.ID)
			matchedCur[s.ID] = true
		} else {
			rep.Removed = append(rep.Removed, s.ID)
		}
	}
	for _, s := range cur {
		if !matchedCur[s.ID] {
			rep.Added = append(rep.Added, s.ID)
		}
	}
	removed := map[string]bool{}
	for _, id := range rep.Removed {
		removed[id] = true
	}
	changed := map[string]bool{}
	for _, id := range rep.Changed {
		changed[id] = true
	}
	for id := range changedOld {
		changed[id] = true
	}
	// A repeated --apply can see provenance already pointing at cur. Map
	// that identity back through this comparison instead of interpreting a
	// reused positional ID as a different statement in old.
	originByCurrent := map[string]string{}
	for _, s := range old {
		if target, ok := rep.Moved[s.ID]; ok {
			originByCurrent[target] = s.ID
		} else if !removed[s.ID] {
			if _, ambiguous := rep.Ambiguous[s.ID]; !ambiguous {
				originByCurrent[s.ID] = s.ID
			}
		}
	}
	if reg != nil {
		for _, x := range reg.Rules {
			if !x.Live() {
				continue
			}
			s := score.StatementFor(x.ID, old, reg)
			if c, ok := curByID[x.StatementID]; ok && x.Source == c.Source && x.StatementHash == translate.TextHash(c.Prose) {
				var belongsToOld bool
				s, belongsToOld = originByCurrent[c.ID]
				if !belongsToOld {
					continue // already translated from a newly added statement
				}
			}
			switch {
			case removed[s]:
				rep.Retire = append(rep.Retire, x.ID)
			case changed[s]:
				key := s
				if to, ok := rep.Moved[s]; ok {
					key = to // the statement's rules are listed under its new ID
				}
				rep.RulesOf[key] = append(rep.RulesOf[key], ir.Pin{Rule: x.ID, Version: x.Version}.String())
			}
			if _, ambiguous := rep.Ambiguous[s]; ambiguous || removed[s] {
				continue
			}
			if moved, ok := rep.Moved[s]; ok {
				s = moved
			}
			if c, ok := curByID[s]; ok && (x.StatementID != c.ID || x.Source != c.Source || x.StatementHash != translate.TextHash(c.Prose)) {
				rep.Sources[x.ID] = RuleSource{StatementID: c.ID, Doc: c.Source, Hash: translate.TextHash(c.Prose)}
			}
		}
	}
	sort.Strings(rep.Retire)
	return rep
}

// Body is a statement's text without the title line a spec tool puts first
// ("Requirement: <title>"), so a renamed requirement with the same content
// reads as unchanged.
func Body(prose string) string {
	first, rest, ok := strings.Cut(prose, "\n")
	if strings.HasPrefix(first, "Requirement: ") {
		if !ok {
			return ""
		}
		return rest
	}
	return prose
}

// Similarity is the share of words two statement bodies have in common
// (Jaccard over lower-cased words): 1 for the same text, 0 for none shared.
func Similarity(a, b string) float64 {
	words := func(s string) map[string]bool {
		out := map[string]bool{}
		for _, w := range strings.FieldsFunc(strings.ToLower(Body(s)), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}) {
			out[w] = true
		}
		return out
	}
	wa, wb := words(a), words(b)
	if len(wa) == 0 && len(wb) == 0 {
		return 1
	}
	common := 0
	for w := range wa {
		if wb[w] {
			common++
		}
	}
	return float64(common) / float64(len(wa)+len(wb)-common)
}

// matchOccurrences gives identical text in the same file an identity by its
// occurrence order, independent of shifted line anchors. Different occurrence
// counts cannot establish which duplicate was inserted or removed.
func matchOccurrences(old, cur []translate.Statement, matchedOld, matchedCur map[string]bool, rep *SyncReport) {
	type occurrence struct {
		stmt translate.Statement
		line int
	}
	group := func(stmts []translate.Statement, matched map[string]bool) map[string][]occurrence {
		groups := map[string][]occurrence{}
		for _, s := range stmts {
			if matched[s.ID] {
				continue
			}
			file, line, err := splitSource(s.Source)
			if err == nil {
				key := file + "\x00" + translate.TextHash(s.Prose)
				groups[key] = append(groups[key], occurrence{s, line})
			}
		}
		for _, xs := range groups {
			sort.Slice(xs, func(i, j int) bool { return xs[i].line < xs[j].line })
		}
		return groups
	}
	before, after := group(old, matchedOld), group(cur, matchedCur)
	for key, xs := range before {
		ys := after[key]
		if len(ys) == 0 {
			continue
		}
		if len(xs) != len(ys) && (len(xs) > 1 || len(ys) > 1) {
			var targets []string
			for _, y := range ys {
				targets = append(targets, y.stmt.ID)
			}
			for _, x := range xs {
				rep.Ambiguous[x.stmt.ID] = targets
				matchedOld[x.stmt.ID] = true
			}
			continue
		}
		for i, x := range xs {
			y := ys[i]
			matchedOld[x.stmt.ID], matchedCur[y.stmt.ID] = true, true
			if x.stmt.ID == y.stmt.ID {
				rep.Unchanged++
			} else {
				rep.Moved[x.stmt.ID] = y.stmt.ID
			}
		}
	}
}

// RetireRules sets status: retired on each rule, in the file that declares it.
// Rules are never deleted, so their history and decisions stay (docs/08).
func RetireRules(reg *ir.Registry, ids []string) error {
	byFile := map[string]map[string]bool{}
	for _, id := range ids {
		x := reg.Rule(id)
		if x == nil {
			return fmt.Errorf("no rule %s", id)
		}
		file := x.File
		if i := strings.LastIndexByte(file, ':'); i > 0 {
			file = file[:i]
		}
		if byFile[file] == nil {
			byFile[file] = map[string]bool{}
		}
		byFile[file][id] = true
	}
	for file, want := range byFile {
		src, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		lines := strings.Split(string(src), "\n")
		var out []string
		done := map[string]bool{}
		for i := 0; i < len(lines); i++ {
			line := lines[i]
			id, ok := idOf(line)
			if !ok || !want[id] {
				out = append(out, line)
				continue
			}
			// Copy this document, replacing or adding its status.
			out = append(out, line)
			j := i + 1
			set := false
			for ; j < len(lines) && strings.TrimSpace(lines[j]) != "---"; j++ {
				if strings.HasPrefix(lines[j], "status:") {
					out = append(out, "status: retired")
					set = true
					continue
				}
				out = append(out, lines[j])
			}
			if !set {
				// Insert right after the id line.
				k := len(out) - (j - i - 1)
				out = append(out[:k], append([]string{"status: retired"}, out[k:]...)...)
			}
			done[id] = true
			i = j - 1
		}
		for id := range want {
			if !done[id] {
				return fmt.Errorf("%s: could not find rule %s to retire", file, id)
			}
		}
		if err := os.WriteFile(file, []byte(strings.Join(out, "\n")), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// idOf reads a top-level `id:` line.
func idOf(line string) (string, bool) {
	if !strings.HasPrefix(line, "id:") {
		return "", false
	}
	var m map[string]string
	if err := yaml.Unmarshal([]byte(line), &m); err != nil {
		return "", false
	}
	id, ok := m["id"]
	return id, ok
}
