package formalize

import (
	"strings"
	"testing"
)

func TestOnePassPrompt(t *testing.T) {
	for _, want := range []string{"# Part 1: vocabulary", "# What vocabulary is", "set_by is world", "# Part 2: rules", "# Rule kinds", "Propose what a statement needs", "# Vocabulary so far"} {
		if !strings.Contains(onePassPrompt, want) {
			t.Errorf("one-pass prompt lacks %q", want)
		}
	}
	if strings.Contains(onePassPrompt, "# Current vocabulary") || strings.Contains(onePassPrompt, "list the missing terms. Never") {
		t.Error("one-pass prompt kept a part it should replace")
	}
	if s, err := combinedSchema(); err != nil || !strings.Contains(s, `"rules"`) || !strings.Contains(s, `"operations"`) {
		t.Errorf("schema: %v %s", err, s)
	}
}
