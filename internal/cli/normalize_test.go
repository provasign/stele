package cli

import (
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
)

func TestNameWords(t *testing.T) {
	got := strings.Join(nameWords("idleShutdownWarningShown"), ",")
	if got != "idle,shutdown,warning,shown" {
		t.Errorf("nameWords = %s", got)
	}
}

func TestSimilarGroupsAcrossEntities(t *testing.T) {
	ws := &ir.Entity{Name: "Workspace"}
	ss := &ir.Entity{Name: "Session"}
	attrs := []*ir.Attr{{Entity: ws, Name: "idleShutdownWarningShown"}, {Entity: ss, Name: "shutdownWarningDisplayed"}, {Entity: ws, Name: "vmExists"}}
	groups := similarGroups(attrs, 8)
	if len(groups) != 1 || len(groups[0]) != 2 {
		t.Fatalf("groups = %v", groups)
	}
}
