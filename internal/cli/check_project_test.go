package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/translate"
)

// TestCheckProject: two changes approved apart, each consistent with what
// its branch held, contradict each other once they meet in one registry;
// `stele check` in the project says so in the requirements' words.
func TestCheckProject(t *testing.T) {
	dir := doorProject(t, false)
	rule := func(file, id, stmt, prose, assert, by string) {
		h := translate.TextHash(prose)
		src, _ := json.Marshal(map[string]string{"statementId": stmt, "doc": stmt, "textHash": h, "statementHash": h, "prose": prose, "approvedBy": by, "approvedHash": h})
		if err := os.WriteFile(filepath.Join(dir, "registry", file), []byte("id: "+id+"\nkind: invariant\nassert: "+assert+"\nsource: "+string(src)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rule("change-a.yaml", "R1", "intent/a", "Doors stay locked\nThe door must always be locked.", "door.locked", "Ana")
	run := func() (int, string) {
		wd, _ := os.Getwd()
		defer os.Chdir(wd)
		os.Chdir(dir)
		var out, errOut bytes.Buffer
		code := checkCmd(nil, &out, &errOut)
		return code, out.String() + errOut.String()
	}
	if code, out := run(); code != 0 || !strings.Contains(out, "1 approved requirement(s)") || !strings.Contains(out, "No two contradict.") {
		t.Fatalf("one change: %d\n%s", code, out)
	}

	// The other branch's change arrives with a merge.
	rule("change-b.yaml", "R2", "intent/b", "Doors open for cleaning\nThe door must never be locked.", "not door.locked", "Ben")
	code, out := run()
	if code != 1 {
		t.Fatalf("after the merge: %d\n%s", code, out)
	}
	for _, want := range []string{`"Doors stay locked" (intent/a, approved by Ana)`, `"Doors open for cleaning" (intent/b, approved by Ben)`, "cannot both hold", "Next: "} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
