package formalize

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// TestDebugPairs prints the candidate pairs for a finished run named by
// STELE_DEBUG_RUN (with STELE_DEBUG_STATEMENTS); it is skipped otherwise.
func TestDebugPairs(t *testing.T) {
	dir := os.Getenv("STELE_DEBUG_RUN")
	if dir == "" {
		t.Skip()
	}
	stmts, err := translate.LoadStatements(os.Getenv("STELE_DEBUG_STATEMENTS"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := loadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := vocab.Load(vocab.Options{VocabFiles: []string{filepath.Join(dir, "context.yaml")}}, st.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	prose := map[string]string{}
	for _, s := range stmts {
		prose[s.ID] = s.Prose
	}
	readers := map[string][]string{}
	var names []string
	rules := parseRules(reg, st.Cands)
	for _, r := range rules {
		for _, a := range r.reads {
			if readers[a.QName()] == nil {
				names = append(names, a.QName())
			}
			readers[a.QName()] = append(readers[a.QName()], r.cand.StatementID)
		}
	}
	var used []vocab.Used
	for _, n := range names {
		u := vocab.Used{Name: n}
		for _, id := range readers[n] {
			u.Text += " " + prose[id]
		}
		used = append(used, u)
	}
	pairs := vocab.SimilarPairs(used, 4, 0.15)
	co := coAsserted(rules, used)
	fmt.Println(len(co), "co-asserted pairs")
	pairs = append(pairs, co...)
	fmt.Println(len(used), "attributes,", len(pairs), "pairs")
	for _, p := range pairs {
		fmt.Println(used[p[0]].Name, "~", used[p[1]].Name)
	}
}
