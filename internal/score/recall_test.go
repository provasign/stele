package score

import (
	"testing"

	"github.com/provasign/stele/internal/translate"
)

func TestRecall(t *testing.T) {
	stmts := []translate.Statement{{ID: "a", Source: "s#L1"}, {ID: "b", Source: "s#L2"}, {ID: "c", Source: "s#L3"}}
	known := []Known{
		{ID: "K1", Sources: []string{"s#L1", "s#L2"}},
		{ID: "K2", Sources: []string{"s#L3", "t#L9"}}, // t#L9 was never segmented
	}
	findings := []Finding{
		{Sources: []string{"s#L2", "s#L3"}},
		{Sources: []string{"AX1", "s#L1", "s#L2", "s#L3"}}, // cites more than K1: still finds it
	}
	r := Recall("x", known, findings, stmts)
	if r.Found != 1 || !r.Known[0].Found || r.Known[0].By != 2 || r.Known[1].Found || !r.Known[1].Unsegmented {
		t.Errorf("recall = %+v", r.Known)
	}
	if r.Matched != 1 {
		t.Errorf("matched = %d, want 1", r.Matched)
	}
}
