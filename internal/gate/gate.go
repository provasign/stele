// Package gate decides whether a registry change may merge (gate 1 in
// docs/08-workflows.md). It compares a base registry with a head registry, as
// CI compares a pull request's base and head, and enforces:
//
//   - the head compiles: every rule and decision resolves and type-checks;
//   - rules are never deleted, only retired, and an edited rule bumps its version;
//   - decisions pin current rule versions (the loader rejects stale ones);
//   - the head has no conflict and no dead response after decisions, other
//     than those an open conflict pins exactly (D-044); adding or re-pinning
//     an open conflict needs the owners' approval, and deleting one is an error;
//   - every vocabulary change that can change what an existing rule means is
//     reported with the rules it affects, for the context owner to approve
//     (docs/08 W3, D-019). Adding entities, attributes, relations or
//     operations is a conservative extension and needs no approval.
package gate

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

// Change is one difference between base and head.
type Change struct {
	Kind     string   `json:"kind"`
	Symbol   string   `json:"symbol"`
	Detail   string   `json:"detail,omitempty"`
	Affected []string `json:"affected_rules,omitempty"`
}

// Result is the gate's decision.
type Result struct {
	Accepted bool `json:"accepted"`
	// Errors block the merge: compile errors, deleted rules, edits without a
	// version bump. When there are errors the check is not run.
	Errors []string        `json:"errors,omitempty"`
	Check  *checker.Result `json:"check,omitempty"`
	Rules  []Change        `json:"rule_changes,omitempty"`
	Vocab  []Change        `json:"vocabulary_changes,omitempty"`
	// NeedsApproval lists vocabulary changes that can change the meaning of
	// existing rules. They do not block the machine check; the context owner
	// approves them in review.
	NeedsApproval []Change `json:"needs_approval,omitempty"`
	// Open lists the open conflicts and what this check found for each; the
	// conflicts they tolerate are not in Check (D-044).
	Open []OpenStatus `json:"open_conflicts,omitempty"`
	// Unapproved lists the live rules this change adds or edits that the
	// owner has not approved for their current statement text (D-056).
	// They block the merge only when the caller requires approval.
	Unapproved []string `json:"unapproved,omitempty"`
	// Pinned lists values that can never hold because one invariant fixes
	// the attribute: the rule's meaning, so they do not fail the check.
	Pinned []checker.DeadResponse `json:"pinned_values,omitempty"`
}

// Dirs gates the change from the registry in baseDir to the one in headDir.
// An empty baseDir means a new registry.
func Dirs(ctx context.Context, z *solver.Z3, baseDir, headDir string) (*Result, error) {
	base := (*ir.Registry)(nil)
	if baseDir != "" {
		var err error
		if base, err = ir.LoadDir(baseDir); err != nil {
			return nil, fmt.Errorf("base does not load (fix main first): %w", err)
		}
	}
	head, err := ir.LoadDir(headDir)
	if err != nil {
		return &Result{Errors: []string{err.Error()}}, nil
	}
	return Registries(ctx, z, base, head)
}

// Registries gates the change from base (nil for a new registry) to head.
func Registries(ctx context.Context, z *solver.Z3, base, head *ir.Registry) (*Result, error) {
	res := &Result{}
	if base != nil {
		res.Rules, res.Errors = ruleChanges(base, head)
		res.Vocab = vocabChanges(base, head)
		for _, c := range res.Vocab {
			if len(c.Affected) > 0 {
				res.NeedsApproval = append(res.NeedsApproval, c)
			}
		}
	}
	res.Unapproved = Unapproved(base, head)
	added, errs := openChanges(base, head)
	res.NeedsApproval = append(res.NeedsApproval, added...)
	res.Errors = append(res.Errors, errs...)
	prog, err := lower.Lower(head, lower.Options{})
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
	}
	if len(res.Errors) > 0 {
		return res, nil
	}
	if res.Check, err = checker.CheckWith(ctx, z, prog, CheckOptions(head)); err != nil {
		return nil, err
	}
	res.Pinned = Pinned(res.Check, head)
	res.Open = Ratchet(res.Check, head)
	res.Accepted = res.Check.Verdict == checker.Pass
	return res, nil
}

// Unapproved lists the live rules of head that base lacks, or that differ
// from base in meaning, version or text, and are not approved for their
// current text. Rules base already had unchanged are grandfathered.
func Unapproved(base, head *ir.Registry) []string {
	var out []string
	for _, h := range head.Rules {
		if !h.Live() || h.Approved() {
			continue
		}
		if base != nil {
			if b := base.Rule(h.ID); b != nil && b.Live() && b.Version == h.Version && fingerprint(b) == fingerprint(h) && b.TextHash == h.TextHash {
				continue
			}
		}
		out = append(out, h.ID)
	}
	sort.Strings(out)
	return out
}

// openChanges lists open conflicts the change adds or re-pins, for the owners
// of every context involved to approve: only an adoption baseline creates
// them (D-044). Deleting one is an error, like deleting a rule: close it.
func openChanges(base, head *ir.Registry) ([]Change, []string) {
	before := map[string]*ir.OpenConflict{}
	if base != nil {
		for _, o := range base.Open {
			before[o.ID] = o
		}
	}
	pins := func(o *ir.OpenConflict) string {
		var ps []string
		for _, p := range o.Rules {
			ps = append(ps, p.String())
		}
		sort.Strings(ps)
		return o.Response + " " + strings.Join(ps, ", ")
	}
	var changes []Change
	for _, o := range head.Open {
		b := before[o.ID]
		delete(before, o.ID)
		switch {
		case b == nil && !o.Closed:
			changes = append(changes, Change{Kind: "open-conflict-added", Symbol: o.ID, Detail: "tolerates " + pins(o), Affected: o.RuleIDs()})
		case b != nil && pins(b) != pins(o) && !o.Closed:
			changes = append(changes, Change{Kind: "open-conflict-repinned", Symbol: o.ID, Detail: pins(b) + " -> " + pins(o), Affected: o.RuleIDs()})
		}
	}
	var errs []string
	for id := range before {
		errs = append(errs, fmt.Sprintf("open conflict %s was deleted; close it instead (status: closed, closedBy: the decision or PR)", id))
	}
	sort.Strings(errs)
	return changes, errs
}

// fingerprint is everything about a rule that an edit can change.
func fingerprint(x *ir.Rule) string { return x.Reading() }

func ruleChanges(base, head *ir.Registry) ([]Change, []string) {
	var changes []Change
	var errs []string
	for _, b := range base.Rules {
		h := head.Rule(b.ID)
		switch {
		case h == nil:
			errs = append(errs, fmt.Sprintf("rule %s was deleted; retire it instead (status: retired), so its history and decisions stay", b.ID))
		case h.Version < b.Version:
			errs = append(errs, fmt.Sprintf("rule %s: version went back from %d to %d", b.ID, b.Version, h.Version))
		case fingerprint(h) != fingerprint(b) && h.Version == b.Version:
			errs = append(errs, fmt.Sprintf("rule %s was edited without a version bump; set version: %d so decisions pinning @%d re-open", b.ID, b.Version+1, b.Version))
		case fingerprint(h) != fingerprint(b):
			changes = append(changes, Change{Kind: "rule-edited", Symbol: b.ID, Detail: fmt.Sprintf("@%d -> @%d", b.Version, h.Version)})
		case h.Status != b.Status:
			changes = append(changes, Change{Kind: "rule-status", Symbol: b.ID, Detail: b.Status + " -> " + h.Status})
		}
	}
	for _, h := range head.Rules {
		if base.Rule(h.ID) == nil {
			changes = append(changes, Change{Kind: "rule-added", Symbol: h.ID})
		}
	}
	return changes, errs
}

// vocabChanges classifies every vocabulary difference. Removals and renames
// need no entry here: dependants stop compiling, which is an error already.
func vocabChanges(base, head *ir.Registry) []Change {
	var out []Change
	names := func(m map[string]*ir.Entity) []string {
		ks := make([]string, 0, len(m))
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}
	for _, en := range names(head.Entities) {
		he, be := head.Entities[en], base.Entities[en]
		if be == nil {
			out = append(out, Change{Kind: "entity-added", Symbol: en})
			continue
		}
		for _, an := range he.AttrNames() {
			ha, ba := he.Attrs[an], be.Attrs[an]
			if ba == nil {
				out = append(out, Change{Kind: "attribute-added", Symbol: ha.QName()})
				continue
			}
			for _, c := range attrChanges(ba, ha) {
				c.Affected = referencing(head, ha)
				out = append(out, c)
			}
		}
		for _, an := range be.AttrNames() {
			if he.Attrs[an] == nil {
				out = append(out, Change{Kind: "attribute-removed", Symbol: be.Name + "." + an})
			}
		}
		for rn := range he.Relations {
			if be.Relations[rn] == nil {
				out = append(out, Change{Kind: "relation-added", Symbol: en + "." + rn})
			}
		}
	}
	for name, hop := range head.Operations {
		bop := base.Operations[name]
		if bop == nil {
			out = append(out, Change{Kind: "operation-added", Symbol: name})
			continue
		}
		out = append(out, opChanges(head, bop, hop)...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

func attrChanges(b, h *ir.Attr) []Change {
	var out []Change
	sym := h.QName()
	if b.Kind != h.Kind {
		return []Change{{Kind: "type-changed", Symbol: sym}}
	}
	if b.Kind == ir.Enum {
		var added, removed []string
		for _, v := range h.Values {
			if !slices.Contains(b.Values, v) {
				added = append(added, v)
			}
		}
		for _, v := range b.Values {
			if !slices.Contains(h.Values, v) {
				removed = append(removed, v)
			}
		}
		if len(added) > 0 {
			// A new value changes every guard such as `status != x` or
			// `not (status in {...})`, and creates states no rule mentions.
			out = append(out, Change{Kind: "enum-values-added", Symbol: sym, Detail: strings.Join(added, ", ")})
		}
		if len(removed) > 0 {
			out = append(out, Change{Kind: "enum-values-removed", Symbol: sym, Detail: strings.Join(removed, ", ")})
		}
	}
	if b.Kind == ir.Int && (b.Min != h.Min || b.Max != h.Max) {
		out = append(out, Change{Kind: "range-changed", Symbol: sym, Detail: fmt.Sprintf("[%d, %d] -> [%d, %d]", b.Min, b.Max, h.Min, h.Max)})
	}
	if b.Controller != h.Controller {
		out = append(out, Change{Kind: "controller-changed", Symbol: sym, Detail: "who decides this value changed, so conflicts are judged differently"})
	}
	return out
}

// referencing lists live rules and decisions in reg that read attribute a.
func referencing(reg *ir.Registry, a *ir.Attr) []string {
	var out []string
	hits := func(subject *ir.Entity, es ...ir.Expr) bool {
		for _, e := range es {
			if e == nil {
				continue
			}
			for _, got := range reg.Reads(e, subject) {
				if got == a {
					return true
				}
			}
		}
		return false
	}
	for _, x := range reg.Rules {
		if !x.Live() {
			continue
		}
		subject, _ := reg.SubjectOf(x)
		if hits(subject, x.When, x.Assert) || (x.Kind == ir.Derivation && a.DefaultBy == x.ID) {
			out = append(out, x.ID)
		}
	}
	for _, d := range reg.Decisions {
		if d.Supersede != nil && hits(nil, d.Supersede.When) {
			// The loser's effective meaning is "unless the decision's condition
			// holds", so it changes too even if its own text does not read a.
			out = append(out, d.ID, d.Supersede.Loser)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// opChanges compares an operation's default, writes and effect. Each changes
// what its rules mean: the default decides when it runs, writes decide which
// attributes its rules read after the step (the rest keep their value), and
// the effect is an obligation of every run.
func opChanges(head *ir.Registry, b, h *ir.Operation) []Change {
	var out []Change
	ofOp := func() []string {
		var ids []string
		for _, x := range head.Rules {
			if x.Live() && x.Op() == h.Name {
				ids = append(ids, x.ID)
			}
		}
		sort.Strings(ids)
		return ids
	}
	if b.Default != h.Default {
		out = append(out, Change{Kind: "default-changed", Symbol: h.Name, Detail: b.Default + " -> " + h.Default, Affected: ofOp()})
	}
	names := func(as []*ir.Attr) []string {
		var ns []string
		for _, a := range as {
			ns = append(ns, a.QName())
		}
		sort.Strings(ns)
		return ns
	}
	bw, hw := names(b.Writes), names(h.Writes)
	if strings.Join(bw, ",") != strings.Join(hw, ",") {
		var changed []string
		for _, n := range hw {
			if !slices.Contains(bw, n) {
				changed = append(changed, n)
			}
		}
		for _, n := range bw {
			if !slices.Contains(hw, n) {
				changed = append(changed, n)
			}
		}
		affected := map[string]bool{}
		for _, n := range changed {
			ent, attr, _ := strings.Cut(n, ".")
			a := head.Entities[ent].Attrs[attr]
			if a == nil {
				continue
			}
			for _, id := range referencing(head, a) {
				x := head.Rule(id)
				// Rules of this operation, and state invariants, read the
				// attribute after this step; their meaning moves with writes.
				if x == nil || x.Op() == h.Name || (x.Kind == ir.Invariant && x.On == "") {
					affected[id] = true
				}
			}
		}
		out = append(out, Change{Kind: "writes-changed", Symbol: h.Name, Detail: strings.Join(bw, ", ") + " -> " + strings.Join(hw, ", "), Affected: keys(affected)})
	}
	effect := func(op *ir.Operation) string {
		var parts []string
		for _, as := range op.Effect {
			parts = append(parts, as.Attr.QName()+"="+as.Value.String())
		}
		if op.From != nil {
			parts = append(parts, "from "+op.From.Attr.QName()+" in {"+strings.Join(op.From.Values, ", ")+"}")
		}
		sort.Strings(parts)
		return strings.Join(parts, ", ")
	}
	if effect(b) != effect(h) {
		out = append(out, Change{Kind: "effect-changed", Symbol: h.Name, Detail: effect(b) + " -> " + effect(h), Affected: ofOp()})
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
