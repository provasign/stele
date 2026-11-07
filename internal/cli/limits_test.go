package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/change"
	"github.com/provasign/stele/internal/gate"
)

// The agent gets one reread of a requirement per change; the owner's notes
// are not limited. A second agent note chased the first on 2026-11-07.
func TestAgentRereadsOnce(t *testing.T) {
	dir := t.TempDir()
	res := map[string]any{"Added": []string{"intent/s1"}, "texts": map[string]string{"intent/s1": "Orders ship within 2 days"}}
	js, _ := json.Marshal(res)
	if err := os.WriteFile(filepath.Join(dir, "result.json"), js, 0o644); err != nil {
		t.Fatal(err)
	}
	reread := func(by string) (int, string) {
		var out bytes.Buffer
		code := cmdChangeReread([]string{"c1", "s1", "--note", "two days is the limit, says " + by, "--by", by, "--out", dir}, &out)
		return code, out.String()
	}
	if code, out := reread("agent"); code != 0 {
		t.Fatalf("first agent reread: %d %s", code, out)
	}
	if code, _ := reread("agent"); code == 0 {
		t.Fatal("a second agent reread of the same requirement was recorded")
	}
	if code, out := reread("owner"); code != 0 {
		t.Fatalf("the owner's reread after the agent's: %d %s", code, out)
	}
	as, _ := change.LoadAnswers(dir)
	if len(as) != 2 || as[0].Quoted || as[1].Quoted {
		t.Fatalf("answers = %+v; want two, neither quoted", as)
	}
}

// Several questions for the owner: the next step says to tell them how
// many before the first.
func TestOwnerQuestionsAreAnnounced(t *testing.T) {
	qs := []change.Question{{ID: "a", AskOwner: true}, {ID: "b", AskOwner: false}, {ID: "c", AskOwner: true}}
	if n := ownerQuestions(qs); n != 2 {
		t.Fatalf("ownerQuestions = %d", n)
	}
	res := &change.Result{Change: "c1", Questions: qs, Gate: &gate.Result{}}
	s := summarize(res, t.TempDir())
	if !strings.Contains(s.Next, "2 questions wait for the owner") {
		t.Fatalf("next = %q", s.Next)
	}
}
