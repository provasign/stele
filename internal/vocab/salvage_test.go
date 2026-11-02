package vocab

import (
	"strings"
	"testing"
)

// A proposal with one bad member keeps everything else.
func TestSalvageKeepsWhatCompiles(t *testing.T) {
	opt := Options{VocabFiles: ctxFile(t), Context: "main"}
	p := Proposal{
		Entities: []Entity{
			{Name: "Order", Attributes: []Attribute{{Name: "paid", Type: "bool"}, {Name: "weird", Type: "no-such-type"}}},
			{Name: "Invoice", Attributes: []Attribute{{Name: "sent", Type: "bool"}}},
		},
		Operations: []Operation{{Name: "Order.pay", Default: "allow", Writes: []string{"Order.paid", "Order.missing"}}},
	}
	if _, err := loadWith(opt, p); err == nil {
		t.Fatal("the fixture should not compile as a whole")
	}
	kept, dropped := salvage(opt, Proposal{}, p)
	if _, err := loadWith(opt, kept); err != nil {
		t.Fatalf("kept proposal does not compile: %v", err)
	}
	got := strings.Join(dropped, ",")
	if got != "Order.weird,Order.pay writes Order.missing" {
		t.Fatalf("dropped = %s", got)
	}
	if len(kept.Entities) != 2 || len(kept.Entities[0].Attributes) != 1 || len(kept.Operations) != 1 || len(kept.Operations[0].Writes) != 1 {
		t.Fatalf("kept = %+v", kept)
	}
}
