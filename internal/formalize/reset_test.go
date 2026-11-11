package formalize

import (
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
)

// TestResetValueFitsItsAttribute: a step resets a world-set value only to a
// fixed value of its type; "now" for a timestamp is no reset (a case study,
// 2026-11-11: it stopped a whole adoption when the step was lowered).
func TestResetValueFitsItsAttribute(t *testing.T) {
	node := &ir.Entity{Name: "Node", Attrs: map[string]*ir.Attr{}}
	at := &ir.Attr{Entity: node, Name: "lastHeartbeatAt", Kind: ir.Time}
	mins := &ir.Attr{Entity: node, Name: "minutesLeft", Kind: ir.Int}
	state := &ir.Attr{Entity: node, Name: "state", Kind: ir.Enum, Values: []string{"idle", "busy"}}
	op := &ir.Operation{Entity: node, Name: "Node.heartbeat"}
	for _, c := range []struct {
		a      *ir.Attr
		assert string
		want   string
	}{
		{at, "self.lastHeartbeatAt = now", ""},
		{mins, "self.minutesLeft = 60", "60"},
		{state, "self.state = idle", "idle"},
		{state, "self.state = gone", ""},
		{mins, "self.minutesLeft = idle", ""},
	} {
		if got := resetValue(&translate.Candidate{Assert: c.assert}, c.a, op); got != c.want {
			t.Errorf("%s: %q, want %q", c.assert, got, c.want)
		}
	}
}
