package formalize

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"

	"github.com/provasign/stele/internal/translate"
)

// bindInputs prevents a resumed run from attaching old rules to changed prose
// or skipping statements after a reorder. Compare vocabulary content, not paths,
// so moving a run and its seed files does not invalidate it.
func bindInputs(dir string, st *state, stmts []translate.Statement, opt Options) error {
	seen := map[string]bool{}
	for _, s := range stmts {
		if s.ID == "" || s.Prose == "" || seen[s.ID] {
			return fmt.Errorf("every statement needs a unique id and prose (%q)", s.ID)
		}
		seen[s.ID] = true
	}
	var vocabulary []string
	for _, path := range opt.VocabFiles {
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		vocabulary = append(vocabulary, string(src))
	}
	data, err := json.Marshal(struct {
		Statements []translate.Statement
		Vocabulary []string
		Context    string
		OnePass    bool
		Compact    bool `json:",omitempty"` // keep existing fingerprints stable
		FactsFirst bool `json:",omitempty"`
	}{stmts, vocabulary, opt.Context, opt.OnePass, opt.Compact, opt.FactsFirst})
	if err != nil {
		return err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	if st.InputHash == "" && st.Stage != "" {
		return fmt.Errorf("%s has no input fingerprint; use a new output directory to preserve the old run", StateFile)
	}
	if st.InputHash != "" && st.InputHash != hash {
		return fmt.Errorf("%s belongs to different inputs (statements, vocabulary, context or translation mode); use the original inputs to resume or a new output directory", StateFile)
	}
	if st.OnePassNext < 0 || st.OnePassNext > len(stmts) {
		return fmt.Errorf("%s has an invalid one-pass position %d for %d statements", StateFile, st.OnePassNext, len(stmts))
	}
	if st.InputHash == "" {
		st.InputHash = hash
		return saveState(dir, st)
	}
	return nil
}
