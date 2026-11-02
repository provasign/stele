package formalize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
)

// Self-conflicts. A statement whose own rules contradict each other is
// almost always a misreading, not a requirement that says two things that
// cannot both hold: on the Project A window one statement's rules clashed
// 67 times and buried every real conflict. Such a clash must never reach the
// owner as a conflict between requirements.
//
// After translation the registry is checked once; every conflict whose rules
// all come from one statement of this run, with no world fact, marks that
// statement. Marked statements are translated once more with the clash
// named, and checked again. A statement still contradicting itself keeps
// its rules out of the registry and ends invalid, with the clash as its
// reason, for the owner to reword or for a later run to read again.

// keepFacts asks a reread to stay on the facts already in use. A reread
// that moved a rule onto newly named facts stopped meeting the requirements
// it conflicts with (held-out NLI-RE Broker, R4 against R2 and R3).
const keepFacts = "Use the facts the current rules use, and facts the shared vocabulary already has, wherever they fit: a new name for a fact other requirements use hides the conflicts between them"

// SelfCheckFile caches the solver's per-unit results between the two checks.
const SelfCheckFile = "self-check-cache.json"

// SelfStats summarizes the self-conflict check.
type SelfStats struct {
	Found     int `json:"found"`     // statements whose rules contradicted each other
	Recovered int `json:"recovered"` // of those, consistent after one more reading
	Excluded  int `json:"excluded"`  // still contradicting: rules held out of the registry
	// Blocking counts statements with a rule that alone leaves a step no
	// valid outcome; Unblocked those that no longer do after one more
	// reading. The rest keep their rules and are listed for the owner.
	Blocking  int    `json:"blocking"`
	Unblocked int    `json:"unblocked"`
	Skipped   string `json:"skipped,omitempty"`
}

// selfCheck finds statements whose rules contradict each other, reads them
// again once, and holds out the rules of those that still do.
func selfCheck(ctx context.Context, m llm.Client, stmts []translate.Statement, st *state, opt Options, dir string, tracked *callJournal) error {
	z := &solver.Z3{Path: opt.Z3}
	if z.Path == "" {
		z.Path = "z3"
	}
	if _, err := exec.LookPath(z.Path); err != nil {
		st.SelfStats = SelfStats{Skipped: "no z3 on PATH"}
		fmt.Fprintf(opt.Log, "self-check: skipped, %s not found\n", z.Path)
		return nil
	}
	ours := map[string]bool{}
	for _, s := range stmts {
		ours[s.ID] = true
	}
	cache := &checker.Cache{Units: map[string]checker.UnitResult{}}
	cachePath := filepath.Join(dir, SelfCheckFile)
	if src, err := os.ReadFile(cachePath); err == nil {
		_ = json.Unmarshal(src, cache)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	find := func() (clashes, blocks map[string][]string, err error) {
		if _, err := finish(dir, m, stmts, st, opt, tracked); err != nil {
			return nil, nil, err
		}
		reg, err := ir.LoadDir(dir)
		if err != nil {
			return nil, nil, err
		}
		prog, err := lower.Lower(reg, lower.Options{})
		if err != nil {
			return nil, nil, fmt.Errorf("self-check: %w", err)
		}
		res, err := checker.CheckWith(ctx, z, prog, checker.Options{Cache: cache})
		if err != nil {
			return nil, nil, fmt.Errorf("self-check: %w", err)
		}
		if js, err := json.Marshal(cache); err == nil {
			_ = os.WriteFile(cachePath, js, 0o644)
		}
		return selfConflicts(reg, res, ours), blockingRules(reg, res, ours), nil
	}
	fmt.Fprintf(opt.Log, "self-check: checking the registry for statements whose rules contradict each other or block a step ...\n")
	found, blocking, err := find()
	if err != nil {
		return err
	}
	st.SelfStats = SelfStats{Found: len(found), Blocking: len(blocking)}
	if len(found)+len(blocking) == 0 {
		fmt.Fprintln(opt.Log, "self-check: none")
		return nil
	}
	notes := map[string]string{}
	for id, why := range found {
		notes[id] = "the current rules contradict each other, which a requirement almost never does: " + strings.Join(why, "; ") +
			". Read the requirement again so its rules can all hold together, keeping every clause it states. " + keepFacts
	}
	for id, why := range blocking {
		n := strings.Join(why, "; ") + ". A rule on a step states what holds after the step, so a rule that asserts only what the step does not set can only forbid the step." +
			" If the text forbids the step in that situation, write a precondition or a deny permission on the step instead. If the text requires the system to produce something (a message, a record, a state), assert a fact the step sets, proposing it and adding it to the step's writes." +
			" If the requirement holds across every step and no fact a step sets can express it (a quality of every error message, say), return that part as informal with a rationale rather than a rule that blocks steps. Keep every other clause the text states. " + keepFacts
		if notes[id] != "" {
			notes[id] += ". Also, " + n
		} else {
			notes[id] = n
		}
	}
	ids := make([]string, 0, len(notes))
	for id := range notes {
		notes[id] += ". Return every part of this statement"
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Fprintf(opt.Log, "self-check: %d statement(s) contradict themselves, %d have a rule that blocks a step (%s); reading them again ...\n", len(found), len(blocking), strings.Join(ids, ", "))
	before := map[string][]translate.Candidate{}
	for _, c := range st.Cands {
		if _, ok := notes[c.StatementID]; ok {
			before[c.StatementID] = append(before[c.StatementID], c)
		}
	}
	if err := retranslate(ctx, m, stmts, ids, notes, st, opt, dir, nil); err != nil {
		return err
	}
	still, stillBlocking, err := find()
	if err != nil {
		return err
	}
	// A reading that compiled in its request can still fail against the
	// final vocabulary. A reread must never leave a statement worse off:
	// such a statement keeps its earlier rules.
	var broken []string
	for _, id := range ids {
		for _, c := range st.Cands {
			if c.StatementID == id && c.Outcome == "rule" && c.Error != "" {
				broken = append(broken, id)
				break
			}
		}
	}
	if len(broken) > 0 {
		fmt.Fprintf(opt.Log, "self-check: the new reading of %s does not compile against the final vocabulary; keeping the earlier rules\n", strings.Join(broken, ", "))
		order := map[string]int{}
		for i, s := range stmts {
			order[s.ID] = i
		}
		st.Cands = slices.DeleteFunc(st.Cands, func(c translate.Candidate) bool { return slices.Contains(broken, c.StatementID) })
		for _, id := range broken {
			st.Cands = append(st.Cands, before[id]...)
		}
		sort.SliceStable(st.Cands, func(i, j int) bool { return order[st.Cands[i].StatementID] < order[st.Cands[j].StatementID] })
		if still, stillBlocking, err = find(); err != nil {
			return err
		}
	}
	st.SelfConflicts, st.Blocking = map[string]string{}, map[string]string{}
	for id := range found {
		if why, ok := still[id]; ok {
			st.SelfConflicts[id] = strings.Join(why, "; ")
			st.SelfStats.Excluded++
		} else {
			st.SelfStats.Recovered++
		}
	}
	for id := range blocking {
		if _, ok := stillBlocking[id]; !ok {
			st.SelfStats.Unblocked++
		}
	}
	// A retranslation can make another statement contradict itself.
	for id, why := range still {
		if _, ok := found[id]; !ok {
			st.SelfConflicts[id] = strings.Join(why, "; ")
			st.SelfStats.Found++
			st.SelfStats.Excluded++
		}
	}
	for id, why := range stillBlocking {
		st.Blocking[id] = strings.Join(why, "; ")
		if _, ok := blocking[id]; !ok {
			st.SelfStats.Blocking++
		}
	}
	fmt.Fprintf(opt.Log, "self-check: %d now consistent, %d still contradict themselves and are held out; %d no longer block a step, %d still do and are listed for the owner\n",
		st.SelfStats.Recovered, st.SelfStats.Excluded, st.SelfStats.Unblocked, len(st.Blocking))
	return nil
}

// selfConflicts lists, for each statement in ours, the clashes among its own
// rules: conflicts of two or more rules that all belong to it, with no
// world fact.
func selfConflicts(reg *ir.Registry, res *checker.Result, ours map[string]bool) map[string][]string {
	out := map[string][]string{}
	for _, cf := range res.Conflicts {
		if len(cf.Rules) < 2 {
			// One rule that leaves a step no valid outcome blocks the step;
			// that can be what the text means ("MUST NOT be cancelled after
			// it expires"), and it is not two readings contradicting.
			continue
		}
		stmt := ""
		one := true
		for _, r := range cf.Rules {
			x := reg.Rule(r)
			if x == nil || x.StatementID == "" || (stmt != "" && x.StatementID != stmt) {
				one = false
				break
			}
			stmt = x.StatementID
		}
		if !one || !ours[stmt] {
			continue
		}
		var b strings.Builder
		if cf.Unit != "" && cf.Unit != "state" {
			fmt.Fprintf(&b, "when %s runs", cf.Unit)
		}
		if s := checker.Plain(checker.Situation(reg, cf.Counterexample)); s != "" {
			if b.Len() > 0 {
				b.WriteString(" and ")
			} else {
				b.WriteString("when ")
			}
			b.WriteString(s)
		}
		if b.Len() > 0 {
			b.WriteString(", these rules cannot all hold: ")
		} else {
			b.WriteString("these rules cannot all hold in any state: ")
		}
		var rules []string
		for _, r := range cf.Rules {
			rules = append(rules, "["+r+": "+ruleText(reg.Rule(r))+"]")
		}
		b.WriteString(strings.Join(rules, " "))
		if len(out[stmt]) < 3 && !slices.Contains(out[stmt], b.String()) {
			out[stmt] = append(out[stmt], b.String()) // a few clashes say enough
		}
	}
	return out
}

// blockingRules lists, for each statement in ours, its rules that alone
// leave a step no valid outcome, with the steps and, when it is so, that
// the rule asserts only facts the step does not set.
func blockingRules(reg *ir.Registry, res *checker.Result, ours map[string]bool) map[string][]string {
	steps := map[string][]string{} // rule -> steps it blocks
	var order []string
	for _, cf := range res.Conflicts {
		if len(cf.Rules) != 1 {
			continue
		}
		r := cf.Rules[0]
		x := reg.Rule(r)
		if x == nil || !ours[x.StatementID] || cf.Unit == "" || cf.Unit == "state" {
			continue
		}
		if x.Kind != ir.Invariant || x.On == "" {
			// Only a rule on a step states what holds after it. A permission
			// the step cannot use ("brokers can delete invoices" against a
			// default deny) is a finding about the specs, not a misreading,
			// and reading it again turned one into an outcome and lost a
			// real conflict (held-out NLI-RE Broker, R42 against R40).
			continue
		}
		if steps[r] == nil {
			order = append(order, r)
		}
		if !slices.Contains(steps[r], cf.Unit) {
			steps[r] = append(steps[r], cf.Unit)
		}
	}
	out := map[string][]string{}
	for _, r := range order {
		x := reg.Rule(r)
		ss := steps[r]
		var b strings.Builder
		fmt.Fprintf(&b, "rule [%s: %s] leaves ", r, ruleText(x))
		if len(ss) == 1 {
			fmt.Fprintf(&b, "step %s", ss[0])
		} else if len(ss) <= 3 {
			fmt.Fprintf(&b, "steps %s", strings.Join(ss, ", "))
		} else {
			fmt.Fprintf(&b, "%d steps (%s, ...)", len(ss), strings.Join(ss[:3], ", "))
		}
		b.WriteString(" no valid outcome in some situations")
		if unset := unwritten(reg, x, ss); len(unset) > 0 {
			fmt.Fprintf(&b, ": it asserts %s, which the step does not set", strings.Join(unset, " and "))
		}
		out[x.StatementID] = append(out[x.StatementID], b.String())
	}
	return out
}

// unwritten names what rule x asserts that none of the steps writes: facts
// the world sets, the request and the actor, and attributes outside the
// steps' writes. It returns nothing if the assertion reads a fact one of
// the steps sets.
func unwritten(reg *ir.Registry, x *ir.Rule, steps []string) []string {
	if x.Assert == nil {
		return nil
	}
	subject, err := reg.SubjectOf(x)
	if err != nil {
		return nil
	}
	var names []string
	for _, a := range reg.Reads(x.Assert, subject) {
		for _, s := range steps {
			if op := reg.Operations[s]; op != nil && op.WritesAttr(a) && !a.WorldSet {
				return nil
			}
		}
		why := a.QName()
		switch {
		case a.WorldSet:
			why += " (set by the world)"
		case a.Entity.Name == "Request" || a.Entity.Name == "Actor":
			why += " (an input)"
		}
		if !slices.Contains(names, why) {
			names = append(names, why)
		}
	}
	return names
}

// ruleText renders a registry rule like describeCandidate renders a
// candidate.
func ruleText(x *ir.Rule) string {
	head := x.Kind
	if x.On != "" {
		head += " on " + x.On
	}
	if x.Operation != "" {
		head += " " + x.Effect + " " + x.Operation
	}
	if x.Subject != "" {
		head += " (self: " + x.Subject + ")"
	}
	if x.When != nil {
		head += " when `" + x.When.String() + "`"
	}
	if x.Assert != nil {
		head += " assert `" + x.Assert.String() + "`"
	}
	return head
}

// holdOut marks the rules of statements that contradict themselves as
// errors, so they are reported and not written. It runs after every
// revalidation, which clears errors that are not compile errors.
func holdOut(cands []translate.Candidate, self map[string]string) {
	for i := range cands {
		c := &cands[i]
		if why, ok := self[c.StatementID]; ok && c.Outcome == "rule" && c.Error == "" {
			c.Error = "its rules contradict each other: " + why
		}
	}
}

// SelfCheckSummary renders the self-conflict check for the owner.
func SelfCheckSummary(res *Result) string {
	s := res.SelfCheck
	switch {
	case s.Skipped != "":
		return "Self-conflict check skipped: " + s.Skipped + "."
	case s.Found == 0 && s.Blocking == 0:
		return ""
	}
	out := ""
	if s.Found > 0 {
		out = fmt.Sprintf("Self-conflict check: %d statements read as contradicting themselves; %d consistent after one more reading, %d still contradicting, their rules held out of the registry (listed as invalid).", s.Found, s.Recovered, s.Excluded)
	}
	if s.Blocking > 0 {
		if out != "" {
			out += " "
		}
		out += fmt.Sprintf("%d statements had a rule that alone leaves a step no valid outcome; %d no longer do after one more reading, %d still do (listed in coverage.md).", s.Blocking, s.Unblocked, s.Blocking-s.Unblocked)
	}
	return out
}
