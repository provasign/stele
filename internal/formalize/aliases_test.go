package formalize

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

func TestCompactIdentityMergeNeedsNoRetranslation(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	opt.NoConsolidate, opt.MaxCalls = false, 2
	calls := 0
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		calls++
		if calls == 2 {
			return &llm.Reply{JSON: `{"merges":[{"from":"Order.paymentReceived","into":"Order.paid","reason":"the same payment fact"}],"links":[]}`}, nil
		}
		if calls != 1 {
			t.Fatal("identity merge made another model call")
		}
		r := compactFixtureReply(true, compactRule("s1", "order.paymentReceived = true")+","+compactRule("s2", "order.paid = true"))
		r.JSON = strings.Replace(r.JSON, `"attributes":[`, `"attributes":[{"name":"paymentReceived","type":"bool","values":[],"min":0,"max":0,"set_by":"system"},`, 1)
		return r, nil
	})
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil || calls != 2 || len(res.Merges) != 1 || res.Counts[Translated] != 2 {
		t.Fatalf("merge did not finalize locally: %+v, %v, calls=%d", res, err, calls)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Entities["Order"].Attrs["paymentReceived"] != nil || !strings.Contains(reg.Rule("S1").Assert.String(), "order.paid") || reg.Rule("S1").StatementID != stmts[0].ID || reg.Rule("S1").Prose != stmts[0].Prose {
		t.Fatal("alias lost rule meaning or provenance")
	}
}

func TestCompactValueMappingStaysForReview(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	opt.NoConsolidate, opt.MaxCalls = false, 2
	calls := 0
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		calls++
		if calls == 2 {
			return &llm.Reply{JSON: `{"merges":[{"from":"Order.paid","into":"Order.paymentStatus","reason":"paid means captured"}],"links":[]}`}, nil
		}
		if calls > 2 {
			t.Fatal("value mapping triggered speculative retranslation")
		}
		r := compactFixtureReply(true, compactRule("s1", "order.paid = true")+","+compactRule("s2", "order.paymentStatus = captured"))
		r.JSON = strings.Replace(r.JSON, `"attributes":[`, `"attributes":[{"name":"paymentStatus","type":"enum","values":["pending","captured"],"min":0,"max":0,"set_by":"system"},`, 1)
		return r, nil
	})
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil || len(res.Merges) != 0 || len(res.Flags) != 1 || !strings.Contains(res.Flags[0].Reason, "explicit value or controller mapping") {
		t.Fatalf("unsafe mapping was applied or hidden: %+v, %v", res, err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil || reg.Entities["Order"].Attrs["paid"] == nil || reg.Entities["Order"].Attrs["paymentStatus"] == nil {
		t.Fatalf("accepted symbol was dropped: %v", err)
	}
}

func TestAliasRewritesResolvedPathsWithoutTouchingPrefixes(t *testing.T) {
	dir, _, opt := compactFixture(t)
	p := vocab.Proposal{Entities: []vocab.Entity{
		{Name: "Left", Attributes: []vocab.Attribute{{Name: "flag", Type: "bool"}, {Name: "flagged", Type: "bool"}}, Relations: []vocab.Relation{{Name: "right", To: "Right"}}},
		{Name: "Right", Attributes: []vocab.Attribute{{Name: "flag", Type: "bool"}}},
	}}
	reg, err := vocab.Load(vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}, p)
	if err != nil {
		t.Fatal(err)
	}
	from, into := reg.Entities["Right"].Attrs["flag"], reg.Entities["Left"].Attrs["flag"]
	original := []translate.Candidate{{StatementID: "S", RuleID: "S", Outcome: "rule", Kind: "invariant", Subject: "Left", When: "not self.right.flag", Assert: "self.right.flag = self.flagged"}}
	renamed, err := renameAlias(reg, original, from, into)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(renamed[0].When, "left.flag") || !strings.Contains(renamed[0].Assert, "self.flagged") || original[0].When != "not self.right.flag" {
		t.Fatalf("path or original mutated incorrectly: %+v", renamed)
	}
	files, err := writeVocabulary(dir, p, opt)
	if err != nil {
		t.Fatal(err)
	}
	translate.Revalidate(files, renamed)
	if renamed[0].Error != "" {
		t.Fatal(renamed[0].Error)
	}
	a, b := *from, *into
	a.WorldSet, b.Controller = true, ir.System
	if identityDomain(&a, &b) {
		t.Fatal("world/system distinction was erased")
	}
	// No step writing one of them is no reason to keep them apart.
	a.WorldSet, a.Controller = false, ir.Environment
	if !identityDomain(&a, &b) {
		t.Fatal("an unwritten attribute cannot fold into its written twin")
	}
}

func TestAliasPreservesDefaultDerivationPaths(t *testing.T) {
	dir, _, opt := compactFixture(t)
	opt.Log = io.Discard
	p := vocab.Proposal{Entities: []vocab.Entity{{Name: "Config", Attributes: []vocab.Attribute{{Name: "oldRetries", Type: "int", Min: 0, Max: 5}, {Name: "retries", Type: "int", Min: 0, Max: 5}}}}}
	st := &state{Proposal: p, Cands: []translate.Candidate{{StatementID: "S", RuleID: "S", Outcome: "rule", Kind: "derivation", Assert: "config.oldRetries.default = 2"}}}
	if err := compactAliases(st, []vocab.Merge{{From: "Config.oldRetries", Into: "Config.retries", Reason: "same setting"}}, opt, dir); err != nil {
		t.Fatal(err)
	}
	if len(st.Merges) != 1 || st.Cands[0].Error != "" || !strings.Contains(st.Cands[0].Assert, "config.retries.default") {
		t.Fatalf("default alias was lost: %+v", st)
	}
	reg, err := vocab.Load(vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}, st.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	p, removed := prune(reg, st.Proposal, parseRules(reg, st.Cands))
	if len(removed) > 0 || len(p.Entities[0].Attributes) != 1 {
		t.Fatalf("default-only setting was pruned: %+v", removed)
	}
}

// TestKnownAttributeAbsorbsNewName: a run proposes a new name for a fact
// the existing vocabulary already has, which no rule of the run reads.
// Given as Known, the existing attribute is compared with it, and the new
// name is folded into the existing one whichever way the model put the merge.
func TestKnownAttributeAbsorbsNewName(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "seed.yaml")
	if err := os.WriteFile(f, []byte("context: shop\nformatVersion: 1\n---\nentity: Order\ncontext: shop\nattributes:\n  paid: { type: bool, meaning: the order is paid }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stmts := []translate.Statement{{ID: "S1", Source: "a.md#L1", Prose: "Orders MUST be settled before shipping."}}
	opt := Options{VocabFiles: []string{f}, Context: "shop", Compact: true, Batch: 20, Rounds: -1, NoCover: true,
		Known: []vocab.Used{{Name: "Order.paid", Type: "bool", Meaning: "the order is paid", Text: "Orders MUST be paid before shipping."}}}
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		text := req.Turns[0].Text
		switch {
		case strings.Contains(text, "statement_id: s1"):
			return &llm.Reply{JSON: `{"entities":[],"extensions":[{"name":"Order","statement_ids":["s1"],"attributes":[{"name":"settled","type":"bool","values":[],"min":0,"max":0,"meaning":"the order is settled"}],"relations":[]}],"operations":[],"rules":[` + compactRule("s1", "order.settled = true") + `]}`}, nil
		case strings.Contains(text, "pair 1") && strings.Contains(text, "Order.paid") && strings.Contains(text, "Order.settled"):
			return &llm.Reply{JSON: `{"merges":[{"from":"Order.paid","into":"Order.settled","reason":"settled means paid"}],"links":[]}`}, nil
		}
		return &llm.Reply{JSON: `{"merges":[],"links":[],"attributes":[]}`}, nil
	})
	res, err := Run(context.Background(), m, stmts, filepath.Join(dir, "out"), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Merges) != 1 || res.Merges[0].From != "Order.settled" || res.Merges[0].Into != "Order.paid" {
		t.Fatalf("merges = %+v, flags = %+v, errors = %v", res.Merges, res.Flags, res.Errors)
	}
	if len(res.Cands) != 1 || !strings.Contains(res.Cands[0].Assert, "order.paid = true") || len(res.Proposal.Extensions) != 0 {
		t.Fatalf("cands = %+v, proposal = %+v", res.Cands, res.Proposal)
	}
}
