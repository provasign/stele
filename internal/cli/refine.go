package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/formalize"
	"github.com/provasign/stele/internal/gate"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lint"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
)

// cmdRefine checks a formalized registry and, for the rules in its
// conflicts, asks the model whether each rule says what its own statement
// says. Statements with a rule that asserts more than the statement, or
// misreads it, are translated again with the reviewer's reason as a note,
// on top of the rest of the registry, and the result is checked again. A
// rule is judged against its own statement only, never against the rule it
// conflicts with, so a real disagreement between statements stays.
func cmdRefine(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("refine", flag.ExitOnError)
	regDir := fs.String("registry", "", "formalized registry directory (from stele formalize)")
	stmtsPath := fs.String("statements", "", "the statements file it was formalized from")
	outDir := fs.String("out", "", "directory for the refined rounds; the last round-N is the refined registry")
	rounds := fs.Int("rounds", 2, "review and retranslation rounds")
	batch := fs.Int("batch", 25, "rules per review request")
	parallel := fs.Int("parallel", 8, "concurrent model requests")
	maxCalls := fs.Int("max-calls", 0, "retranslation calls allowed per round; 0 is unlimited")
	requestTimeout := fs.Duration("request-timeout", 10*time.Minute, "limit per model call")
	solverTimeout := fs.Duration("solver-timeout", 5*time.Second, "per-query solver budget")
	z3Path := fs.String("z3", "", "path to a z3 binary to run (default: Stele's own Z3, else z3 on PATH)")
	timeout := fs.Duration("timeout", 3*time.Hour, "limit for the run")
	client := llmFlags(fs)
	_ = fs.Parse(args)
	if *regDir == "" || *stmtsPath == "" || *outDir == "" {
		fmt.Fprint(os.Stderr, "usage: stele refine --registry DIR --statements FILE --out DIR [--rounds N] [--llm B]\n")
		return 2
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		return failf("refine", err)
	}
	m, err := client()
	if err != nil {
		return failf("refine", err)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return failf("refine", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	byID := map[string]translate.Statement{}
	for _, s := range stmts {
		byID[s.ID] = s
	}
	z := &solver.Z3{Path: *z3Path, Timeout: *solverTimeout}
	cache := &checker.Cache{}
	report := refineReport{Backend: m.Name()}
	cur := *regDir
	for round := 1; ; round++ {
		reg, res, err := checkDir(ctx, z, cur, cache)
		if err != nil {
			return failf("refine", err)
		}
		fmt.Fprintf(out, "%s: %d conflict(s)\n", cur, len(res.Conflicts))
		report.Rounds = append(report.Rounds, refineRound{Registry: cur, Conflicts: len(res.Conflicts)})
		if len(res.Conflicts) == 0 || round > *rounds {
			break
		}
		// A conflict among the rules of one statement is a translation
		// error whatever the reviewer says: translate it again, with the
		// situation and any lint finding as the reason.
		self := map[string][]string{}
		findings := lint.Rules(reg)
		why := map[string]string{}
		for _, f := range findings {
			why[f.Rule] = f.Message
		}
		var sets [][]string
		for _, c := range res.Conflicts {
			sets = append(sets, c.Rules)
		}
		for _, i := range lint.SelfConflicts(reg, sets) {
			c := res.Conflicts[i]
			x := reg.Rule(c.Rules[0])
			sid := x.Statement()
			if _, ok := self[sid]; ok || sid == "" {
				continue
			}
			var texts, reasons []string
			for _, id := range c.Rules {
				if r := reg.Rule(id); r != nil {
					texts = append(texts, ruleText(r))
					if why[id] != "" {
						reasons = append(reasons, why[id])
					}
				}
			}
			note := fmt.Sprintf("review: the rules translated from this statement (%s) cannot all hold in the situation %s, so the statement alone would leave the system no valid response.", strings.Join(texts, "; "), strings.Join(situation(reg, c.Counterexample), ", "))
			if len(reasons) > 0 {
				note += " " + strings.Join(reasons, " ")
			}
			self[sid] = []string{note + " Translate it again so that its rules can all hold wherever its conditions hold, asserting only what it says."}
		}
		// A linted rule in a conflict has a shape that is a translation error
		// (it constrains what its operation does not write): the same.
		for _, c := range res.Conflicts {
			for _, id := range c.Rules {
				x := reg.Rule(id)
				if x == nil || why[id] == "" {
					continue
				}
				if sid := x.Statement(); sid != "" {
					if _, ok := self[sid]; !ok {
						self[sid] = []string{fmt.Sprintf("review: the rule %s cannot be made to hold by its operation: %s Translate the statement again so that it says what the statement says about the operation's result, and puts what it only reads in \"when\".", ruleText(x), why[id])}
					}
				}
			}
		}
		items := conflictRules(reg, res)
		start := time.Now()
		judged, err := judgeRules(ctx, m, reg, byID, items, *batch, *parallel, *requestTimeout, filepath.Join(*outDir, fmt.Sprintf("review-%d", round)))
		if err != nil {
			return failf("refine", err)
		}
		bad := map[string][]string{}
		counts := map[string]int{}
		for _, j := range judged {
			counts[j.Verdict]++
			if j.Verdict == "over" || j.Verdict == "wrong" {
				bad[j.Statement] = append(bad[j.Statement], fmt.Sprintf("review: an earlier translation gave the rule %s, which does not say what this statement says (%s): %s Translate the statement again, asserting only what it says, where it says it.", j.Text, j.Verdict, j.Reason))
			}
		}
		for sid, notes := range self {
			if _, ok := bad[sid]; !ok {
				bad[sid] = notes
			}
		}
		r := &report.Rounds[len(report.Rounds)-1]
		r.SelfConflicting = len(self)
		facts, falseFacts, err := reviewSituations(ctx, m, reg, res, *parallel, *requestTimeout, filepath.Join(*outDir, fmt.Sprintf("situations-%d", round)))
		if err != nil {
			return failf("refine", err)
		}
		prec, err := reviewPrecedence(ctx, m, reg, res, byID, *parallel, *requestTimeout, filepath.Join(*outDir, fmt.Sprintf("precedence-%d", round)))
		if err != nil {
			return failf("refine", err)
		}
		for sid := range bad {
			delete(prec, sid) // translated again this round; a later round can add it
		}
		r.Precedence = prec
		r.Reviewed, r.Verdicts, r.Judgments, r.ReviewSeconds = len(judged), counts, judged, time.Since(start).Seconds()
		fmt.Fprintf(out, "  reviewed %d rule(s) in %.0fs: %d faithful, %d over, %d wrong; %d self-conflicting statement(s); %d statement(s) to translate again\n",
			len(judged), r.ReviewSeconds, counts["faithful"], counts["over"], counts["wrong"], len(self), len(bad))
		if len(bad) == 0 && len(facts) == 0 && len(falseFacts) == 0 && len(prec) == 0 {
			break
		}
		next := filepath.Join(*outDir, fmt.Sprintf("round-%d", round))
		files, err := carryOver(cur, next, round, bad, falseFacts, prec)
		if err != nil {
			return failf("refine", err)
		}
		factRound := max(round, nextFactRound(next))
		kept, err := addFacts(next, factRound, facts)
		if err != nil {
			return failf("refine", err)
		}
		if len(kept) > 0 {
			files = append(files, filepath.Join(next, fmt.Sprintf("facts-%d.yaml", factRound)))
		}
		r.Facts, r.FalseFacts = kept, falseFacts
		fmt.Fprintf(out, "  situations: %d world fact(s) proposed, %d kept; %d proposed world fact(s) false; %d statement(s) given the precedence their text states\n", len(facts), len(kept), len(falseFacts), len(prec))
		if len(bad) == 0 {
			cur = next
			continue
		}
		var again []translate.Statement
		for _, s := range stmts {
			if notes, ok := bad[s.ID]; ok {
				s.Notes = append(append([]string(nil), s.Notes...), notes...)
				again = append(again, s)
			}
		}
		res2, err := formalize.Run(ctx, m, again, next, formalize.Options{VocabFiles: files, Compact: true, Batch: 20, BatchBytes: 12000,
			Parallel: *parallel, Retrieve: 60, MaxCalls: *maxCalls, RequestTimeout: *requestTimeout, Log: out})
		if err != nil {
			return failf("refine", err)
		}
		r.Retranslated = len(again)
		r.Counts = map[string]int{}
		for k, v := range res2.Counts {
			r.Counts[string(k)] = v
		}
		cur = next
	}
	js, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "refine.json"), js, 0o644); err != nil {
		return failf("refine", err)
	}
	fmt.Fprintf(out, "refined registry: %s (see %s)\n", cur, filepath.Join(*outDir, "refine.json"))
	return 0
}

type refineReport struct {
	Backend string        `json:"backend"`
	Rounds  []refineRound `json:"rounds"`
}

type refineRound struct {
	Registry  string `json:"registry"`
	Conflicts int    `json:"conflicts"`
	Reviewed  int    `json:"reviewed,omitempty"`
	// SelfConflicting statements have rules that conflict among themselves.
	SelfConflicting int            `json:"self_conflicting,omitempty"`
	ReviewSeconds   float64        `json:"review_seconds,omitempty"`
	Verdicts        map[string]int `json:"verdicts,omitempty"`
	Retranslated    int            `json:"retranslated,omitempty"`
	Counts          map[string]int `json:"counts,omitempty"` // retranslation outcomes
	Judgments       []judgment     `json:"judgments,omitempty"`
	Facts           []worldFact    `json:"facts,omitempty"`       // world facts added, ruling out impossible situations
	FalseFacts      []string       `json:"false_facts,omitempty"` // proposed world facts removed as not always true
	// Precedence maps a statement to those it overrides, as its own text says.
	Precedence map[string][]string `json:"precedence,omitempty"`
}

type judgment struct {
	Rule      string `json:"rule"`
	Statement string `json:"statement"`
	Text      string `json:"text"`
	Verdict   string `json:"verdict"`
	Reason    string `json:"reason"`
}

func checkDir(ctx context.Context, z *solver.Z3, dir string, cache *checker.Cache) (*ir.Registry, *checker.Result, error) {
	reg, err := ir.LoadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	prog, err := lower.Lower(reg, lower.Options{})
	if err != nil {
		return nil, nil, err
	}
	opt := gate.CheckOptions(reg)
	opt.Cache = cache
	res, err := checker.CheckWith(ctx, z, prog, opt)
	return reg, res, err
}

// reviewItem is one rule found in a conflict, with the first situation in
// which the checker applied it.
type reviewItem struct {
	rule      *ir.Rule
	situation []string
}

func conflictRules(reg *ir.Registry, res *checker.Result) []reviewItem {
	seen := map[string]bool{}
	var items []reviewItem
	for _, c := range res.Conflicts {
		for _, id := range c.Rules {
			x := reg.Rule(id)
			if x == nil || seen[id] || x.Kind == "obligation" || x.Kind == "informal" {
				continue
			}
			seen[id] = true
			items = append(items, reviewItem{x, situation(reg, c.Counterexample)})
		}
	}
	return items
}

func ruleText(x *ir.Rule) string {
	s := x.Kind
	if x.On != "" {
		s += " on " + x.On
	}
	if x.Operation != "" {
		s += " " + x.Effect + " " + x.Operation
	}
	if x.Subject != "" {
		s += " (self: " + x.Subject + ")"
	}
	if x.When != nil {
		s += " when `" + x.When.String() + "`"
	}
	if x.Assert != nil {
		s += " assert `" + x.Assert.String() + "`"
	}
	return s
}

const reviewInstructions = `
# Your task: review translations

You do not translate. Each item below is one rule translated from one
requirement statement, with a situation in which the checker applied it.
Decide only whether the rule says what its own statement says:

- faithful: the statement claims what the rule asserts, in the situations
  the rule applies to. Strict or inconvenient is still faithful.
- over: the rule asserts more than the statement claims. Typical causes: it
  asserts a whole-object or application-wide outcome (loaded, succeeded, not
  rejected, output empty, exited) where the statement names one part's
  handling; it drops a condition or qualifier the statement states, so it
  applies where the statement makes no claim; it constrains every candidate
  where the statement means the selected one; it gives an absent value an
  invented literal; it makes causes that can hold together exclusive values
  of one attribute, so it demands its value even where another cause holds.
- wrong: the rule misreads the statement.

Judge each rule against its own statement only. Another requirement may
contradict it; that is for the spec's owners, and is never a reason to call
a faithful rule over or wrong. For over and wrong, the reason says in one or
two sentences what the statement does and does not claim.
`

const reviewSchema = `{"type":"object","additionalProperties":false,"required":["judgments"],"properties":{"judgments":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["rule","verdict","reason"],"properties":{"rule":{"type":"string"},"verdict":{"type":"string","enum":["faithful","over","wrong"]},"reason":{"type":"string"}}}}}}`

func judgeRules(ctx context.Context, m llm.Client, reg *ir.Registry, byID map[string]translate.Statement, items []reviewItem, batch, parallel int, timeout time.Duration, archive string) ([]judgment, error) {
	lang := translate.SystemPrompt
	if i := strings.Index(lang, "# State and steps"); i >= 0 {
		lang = lang[i:]
	}
	lang = strings.Replace(lang, "Return every statement you were given, each with at least one entry.\n", "", 1)
	system := "Stele rules are typed rules over a vocabulary, translated from software requirements. This is how they are written:\n\n" +
		lang + translate.RenderVocabulary(reg) + reviewInstructions
	if err := os.MkdirAll(archive, 0o755); err != nil {
		return nil, err
	}
	var parts [][]reviewItem
	for i := 0; i < len(items); i += batch {
		parts = append(parts, items[i:min(i+batch, len(items))])
	}
	out := make([][]judgment, len(parts))
	errs := make([]error, len(parts))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for p, part := range parts {
		wg.Add(1)
		go func(p int, part []reviewItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var req strings.Builder
			alias := map[string]reviewItem{}
			for k, it := range part {
				a := fmt.Sprintf("r%d", k+1)
				alias[a] = it
				s := byID[it.rule.StatementID]
				prose := s.Prose
				if prose == "" {
					prose = it.rule.Prose
				}
				fmt.Fprintf(&req, "rule: %s\nstatement: %s\nrule text: %s\nsituation: %s\n\n", a, strings.Join(strings.Fields(prose), " "), ruleText(it.rule), strings.Join(it.situation, ", "))
			}
			var reply *llm.Reply
			var err error
			for attempt := 0; attempt < 2; attempt++ {
				cctx, cancel := context.WithTimeout(ctx, timeout)
				reply, err = m.Complete(cctx, llm.Request{System: system, Turns: []llm.Turn{{Role: "user", Text: req.String()}}, Schema: reviewSchema})
				cancel()
				if err == nil {
					break
				}
			}
			if err != nil {
				errs[p] = err
				return
			}
			var got struct {
				Judgments []struct{ Rule, Verdict, Reason string } `json:"judgments"`
			}
			if err := json.Unmarshal([]byte(reply.JSON), &got); err != nil {
				errs[p] = fmt.Errorf("review reply: %w", err)
				return
			}
			saved, _ := json.MarshalIndent(map[string]any{"request": req.String(), "reply": json.RawMessage(reply.JSON), "usage": reply.Usage}, "", " ")
			_ = os.WriteFile(filepath.Join(archive, fmt.Sprintf("%03d.json", p+1)), saved, 0o644)
			for _, j := range got.Judgments {
				it, ok := alias[j.Rule]
				if !ok {
					continue
				}
				delete(alias, j.Rule)
				out[p] = append(out[p], judgment{Rule: it.rule.ID, Statement: it.rule.StatementID, Text: ruleText(it.rule), Verdict: j.Verdict, Reason: j.Reason})
			}
		}(p, part)
	}
	wg.Wait()
	var all []judgment
	for p := range parts {
		if errs[p] != nil {
			return nil, errs[p]
		}
		all = append(all, out[p]...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Rule < all[j].Rule })
	return all, nil
}

// carryOver copies the registry in dir to next under round-prefixed names,
// as the base of the retranslation: vocabulary files are returned for the
// vocabulary, and the rules of the statements to translate again are left
// out of the rule files.
func carryOver(dir, next string, round int, drop map[string][]string, dropRules []string, overrides map[string][]string) ([]string, error) {
	if err := os.MkdirAll(next, 0o755); err != nil {
		return nil, err
	}
	entries, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var vocab []string
	for _, f := range entries {
		name := filepath.Base(f)
		dst := filepath.Join(next, fmt.Sprintf("r%d-%s", round, name))
		src, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if src, err = dropStatements(src, drop, dropRules); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if src, err = addOverrides(src, overrides); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if !strings.HasSuffix(name, "rules.yaml") {
			vocab = append(vocab, dst)
		}
		if err := os.WriteFile(dst, src, 0o644); err != nil {
			return nil, err
		}
	}
	return vocab, nil
}

// dropStatements removes the YAML documents whose source.statementId is
// in drop or whose id is in dropRules, keeping the others' text as it is.
func dropStatements(src []byte, drop map[string][]string, dropRules []string) ([]byte, error) {
	var out bytes.Buffer
	docs := strings.Split(string(src), "\n---\n")
	for i, d := range docs {
		var doc struct {
			ID     string `yaml:"id"`
			Source struct {
				StatementID string `yaml:"statementId"`
			} `yaml:"source"`
		}
		if err := yaml.Unmarshal([]byte(d), &doc); err != nil {
			return nil, err
		}
		if _, ok := drop[doc.Source.StatementID]; ok && doc.Source.StatementID != "" {
			continue
		}
		if doc.ID != "" && slices.Contains(dropRules, doc.ID) {
			continue
		}
		if out.Len() > 0 && i > 0 {
			out.WriteString("\n---\n")
		}
		out.WriteString(d)
	}
	return out.Bytes(), nil
}

type worldFact struct {
	ID     string `json:"id"`
	Assert string `json:"assert"`
	Reason string `json:"reason"`
	Error  string `json:"error,omitempty"` // why it did not load
}

const situationInstructions = `
# Your task: review situations

You do not translate and you do not see the requirements. Each item is a
situation the checker found: values of attributes of the system above, in
which some requirements cannot all hold. Decide only whether the situation
can happen at all, given what its attributes mean (their names, entities,
types and meanings in the vocabulary).

It cannot happen when its values contradict each other by their nature: a
run that both succeeded and failed, an empty file that has a version, a step
that failed though it never ran, a ratio of 2, a selected item that is not
among the candidates, two names for one event that disagree. A situation
that is only unusual, undesirable, or forbidden by some requirement can
happen: that is what the requirements are about, and it is never a reason
to rule it out.

When it cannot happen, give the world facts that rule it out: invariants
with no operation, true in every real situation by what the attributes
mean, never because a requirement demands it. Write them in the rule
language, with entity roots in lower camel case (order.status = shipped
implies isSet(order.trackingNumber)), not self. Keep each fact as narrow as
the reason it is true.

Each item may list world facts proposed earlier. List the ids of those that
are not true in every real situation by what the attributes mean.
`

const situationSchema = `{"type":"object","additionalProperties":false,"required":["situations"],"properties":{"situations":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["item","possible","facts","reason","false_facts"],"properties":{"item":{"type":"string"},"possible":{"type":"boolean"},"facts":{"type":"array","items":{"type":"string"}},"reason":{"type":"string"},"false_facts":{"type":"array","items":{"type":"string"}}}}}}}`

func isProposedFact(id string) bool {
	return strings.HasPrefix(id, formalize.LinkPrefix) || strings.HasPrefix(id, "fact-")
}

// reviewSituations asks, for each distinct conflict, whether its situation
// can happen at all by what its attributes mean, and returns the world facts
// that rule out those that cannot, and the proposed world facts the model
// says are not always true.
func reviewSituations(ctx context.Context, m llm.Client, reg *ir.Registry, res *checker.Result, parallel int, timeout time.Duration, archive string) ([]worldFact, []string, error) {
	lang := translate.SystemPrompt
	if i := strings.Index(lang, "# Expressions"); i >= 0 {
		if j := strings.Index(lang, "# How to translate"); j > i {
			lang = lang[i:j]
		}
	}
	system := "Stele describes a software system by a vocabulary and checks requirements over it. The vocabulary:\n\n" +
		translate.RenderVocabulary(reg) + "\n" + lang + situationInstructions
	if err := os.MkdirAll(archive, 0o755); err != nil {
		return nil, nil, err
	}
	type item struct {
		lines []string
		facts []*ir.Rule
	}
	seen := map[string]bool{}
	var items []item
	for _, c := range res.Conflicts {
		key := strings.Join(slices.Sorted(slices.Values(c.Rules)), ",")
		if seen[key] || len(items) >= 120 {
			continue
		}
		seen[key] = true
		it := item{lines: situation(reg, c.Counterexample)}
		for _, id := range c.Rules {
			if x := reg.Rule(id); x != nil && isProposedFact(id) {
				it.facts = append(it.facts, x)
			}
		}
		items = append(items, it)
	}
	const per = 20
	var facts []worldFact
	var falseFacts []string
	var mu sync.Mutex
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	var firstErr error
	for p := 0; p*per < len(items); p++ {
		part := items[p*per : min((p+1)*per, len(items))]
		wg.Add(1)
		go func(p int, part []item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var req strings.Builder
			for k, it := range part {
				fmt.Fprintf(&req, "item: i%d\nsituation: %s\n", k+1, strings.Join(it.lines, ", "))
				for _, f := range it.facts {
					fmt.Fprintf(&req, "proposed world fact %s: %s (%s)\n", f.ID, f.Assert.String(), strings.TrimPrefix(f.Prose, "World fact: "))
				}
				req.WriteString("\n")
			}
			var reply *llm.Reply
			var err error
			for attempt := 0; attempt < 2; attempt++ {
				cctx, cancel := context.WithTimeout(ctx, timeout)
				reply, err = m.Complete(cctx, llm.Request{System: system, Turns: []llm.Turn{{Role: "user", Text: req.String()}}, Schema: situationSchema})
				cancel()
				if err == nil {
					break
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			var got struct {
				Situations []struct {
					Item, Reason string
					Possible     bool
					Facts        []string
					FalseFacts   []string `json:"false_facts"`
				} `json:"situations"`
			}
			if err := json.Unmarshal([]byte(reply.JSON), &got); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("situation reply: %w", err)
				}
				return
			}
			saved, _ := json.MarshalIndent(map[string]any{"request": req.String(), "reply": json.RawMessage(reply.JSON), "usage": reply.Usage}, "", " ")
			_ = os.WriteFile(filepath.Join(archive, fmt.Sprintf("%03d.json", p+1)), saved, 0o644)
			for _, s := range got.Situations {
				var shown []string
				var k int
				if _, err := fmt.Sscanf(s.Item, "i%d", &k); err == nil && k >= 1 && k <= len(part) {
					for _, f := range part[k-1].facts {
						shown = append(shown, f.ID)
					}
				}
				for _, id := range s.FalseFacts {
					if slices.Contains(shown, id) && !slices.Contains(falseFacts, id) {
						falseFacts = append(falseFacts, id)
					}
				}
				if s.Possible {
					continue
				}
				for _, f := range s.Facts {
					if f = strings.TrimSpace(f); f != "" && !slices.ContainsFunc(facts, func(w worldFact) bool { return w.Assert == f }) {
						facts = append(facts, worldFact{Assert: f, Reason: s.Reason})
					}
				}
			}
		}(p, part)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, nil, firstErr
	}
	sort.Strings(falseFacts)
	return facts, falseFacts, nil
}

// addFacts writes the world facts that load with the registry in dir to
// facts-<round>.yaml there, one at a time, and returns those it kept.
func addFacts(dir string, round int, facts []worldFact) ([]worldFact, error) {
	kept, _, err := addFactsReport(dir, round, facts)
	return kept, err
}

// nextFactRound is one more than the highest round of the world facts
// already in dir (fact-<round>-<n>), so new facts never reuse an ID.
func nextFactRound(dir string) int {
	reg, err := ir.LoadDir(dir)
	if err != nil {
		return 0
	}
	next := 0
	for _, x := range reg.Rules {
		var r, n int
		if _, err := fmt.Sscanf(x.ID, "fact-%d-%d", &r, &n); err == nil && r >= next {
			next = r + 1
		}
	}
	return next
}

// addFactsReport is addFacts that also returns the facts it left out, each
// with the reason it did not load.
func addFactsReport(dir string, round int, facts []worldFact) ([]worldFact, []worldFact, error) {
	path := filepath.Join(dir, fmt.Sprintf("facts-%d.yaml", round))
	header := "# World facts added by `stele refine`: they rule out situations that cannot happen by what\n# their attributes mean. Review each: keep, edit or delete. A wrong one hides conflicts.\n"
	var kept, rejected []worldFact
	var docs []string
	for i, f := range facts {
		f.ID = fmt.Sprintf("fact-%d-%d", round, i+1)
		doc, err := yaml.Marshal(map[string]any{"id": f.ID, "status": "active", "kind": "invariant", "assert": f.Assert,
			"source": map[string]any{"inferredBy": "stele refine", "prose": "World fact: " + f.Reason}})
		if err != nil {
			return nil, nil, err
		}
		try := append(slices.Clone(docs), string(doc))
		if err := os.WriteFile(path, []byte(header+"---\n"+strings.Join(try, "---\n")), 0o644); err != nil {
			return nil, nil, err
		}
		reg, err := ir.LoadDir(dir)
		if err == nil {
			_, err = lower.Lower(reg, lower.Options{})
		}
		if err != nil {
			f.Error = err.Error()
			rejected = append(rejected, f)
			continue
		}
		docs = try
		kept = append(kept, f)
	}
	if len(docs) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, nil, err
		}
		return nil, rejected, nil
	}
	return kept, rejected, os.WriteFile(path, []byte(header+"---\n"+strings.Join(docs, "---\n")), 0o644)
}

// addOverrides adds to each rule of a statement in overrides the statements
// it overrides, leaving every other document's text as it is.
func addOverrides(src []byte, overrides map[string][]string) ([]byte, error) {
	if len(overrides) == 0 {
		return src, nil
	}
	docs := strings.Split(string(src), "\n---\n")
	for i, d := range docs {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(d), &node); err != nil {
			return nil, err
		}
		if len(node.Content) == 0 || node.Content[0].Kind != yaml.MappingNode {
			continue
		}
		var doc struct {
			Overrides []string `yaml:"overrides"`
			Source    struct {
				StatementID string `yaml:"statementId"`
			} `yaml:"source"`
		}
		if err := node.Decode(&doc); err != nil {
			return nil, err
		}
		add := overrides[doc.Source.StatementID]
		if len(add) == 0 {
			continue
		}
		all := slices.Clone(doc.Overrides)
		for _, x := range add {
			if !slices.Contains(all, x) {
				all = append(all, x)
			}
		}
		m := node.Content[0]
		var val yaml.Node
		if err := val.Encode(all); err != nil {
			return nil, err
		}
		set := false
		for k := 0; k+1 < len(m.Content); k += 2 {
			if m.Content[k].Value == "overrides" {
				m.Content[k+1] = &val
				set = true
			}
		}
		if !set {
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "overrides"}, &val)
		}
		// Keep a leading comment line (a file header) in front of the document.
		var head string
		if strings.HasPrefix(d, "#") {
			if j := strings.Index(d, "\n"); j >= 0 {
				head = d[:j+1]
			}
		}
		out, err := yaml.Marshal(&node)
		if err != nil {
			return nil, err
		}
		docs[i] = head + strings.TrimSuffix(string(out), "\n")
	}
	return []byte(strings.Join(docs, "\n---\n")), nil
}

const precedenceInstructions = `You check software requirement statements for precedence that their own text states.
Each item lists statements whose requirements cannot all hold in some
situation. Decide only whether one of them says, in its own words, that it
takes precedence over another listed one where both apply: it names the
other requirement ("per FR-005, MUST NOT apply when --dry-run is set"),
names the behavior it overrides ("regardless of the --force flag"),
or states itself as an exception to it ("unless", "except when", "even if",
"instead of", "does not apply to").

Being more specific, newer or stricter is not precedence: when no
statement's text says which wins, give no winner, because the owners must
decide. Never use the other statements, or what seems sensible, to decide.

For a winner, quote the exact words of the winning statement that state the
precedence (copied character for character), and list the statements it
takes precedence over.
`

const precedenceSchema = `{"type":"object","additionalProperties":false,"required":["items"],"properties":{"items":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["item","winner","over","quote"],"properties":{"item":{"type":"string"},"winner":{"type":"string"},"over":{"type":"array","items":{"type":"string"}},"quote":{"type":"string"}}}}}}`

// reviewPrecedence asks, for each conflict among two or more statements,
// whether one statement's own text gives it precedence over another. A
// claimed precedence is kept only when its quote is in the winner's text.
func reviewPrecedence(ctx context.Context, m llm.Client, reg *ir.Registry, res *checker.Result, byID map[string]translate.Statement, parallel int, timeout time.Duration, archive string) (map[string][]string, error) {
	if err := os.MkdirAll(archive, 0o755); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var items [][]string
	for _, c := range res.Conflicts {
		var ids []string
		for _, id := range c.Rules {
			if x := reg.Rule(id); x != nil && !isProposedFact(id) {
				if sid := x.Statement(); byID[sid].Prose != "" && !slices.Contains(ids, sid) {
					ids = append(ids, sid)
				}
			}
		}
		sort.Strings(ids)
		key := strings.Join(ids, "\x00")
		if len(ids) < 2 || seen[key] || len(items) >= 150 {
			continue
		}
		seen[key] = true
		items = append(items, ids)
	}
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	out := map[string][]string{}
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	const per = 15
	for p := 0; p*per < len(items); p++ {
		part := items[p*per : min((p+1)*per, len(items))]
		wg.Add(1)
		go func(p int, part [][]string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var req strings.Builder
			for k, ids := range part {
				fmt.Fprintf(&req, "item: i%d\n", k+1)
				for j, id := range ids {
					fmt.Fprintf(&req, "statement s%d: %s\n", j+1, strings.Join(strings.Fields(byID[id].Prose), " "))
				}
				req.WriteString("\n")
			}
			var reply *llm.Reply
			var err error
			for attempt := 0; attempt < 2; attempt++ {
				cctx, cancel := context.WithTimeout(ctx, timeout)
				reply, err = m.Complete(cctx, llm.Request{System: precedenceInstructions, Turns: []llm.Turn{{Role: "user", Text: req.String()}}, Schema: precedenceSchema})
				cancel()
				if err == nil {
					break
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			var got struct {
				Items []struct {
					Item, Winner, Quote string
					Over                []string
				} `json:"items"`
			}
			if err := json.Unmarshal([]byte(reply.JSON), &got); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("precedence reply: %w", err)
				}
				return
			}
			saved, _ := json.MarshalIndent(map[string]any{"request": req.String(), "reply": json.RawMessage(reply.JSON), "usage": reply.Usage}, "", " ")
			_ = os.WriteFile(filepath.Join(archive, fmt.Sprintf("%03d.json", p+1)), saved, 0o644)
			stmtOf := func(ids []string, alias string) string { return statementNamed(ids, alias) }
			for _, g := range got.Items {
				var k int
				if _, err := fmt.Sscanf(g.Item, "i%d", &k); err != nil || k < 1 || k > len(part) {
					continue
				}
				ids := part[k-1]
				win := stmtOf(ids, g.Winner)
				q := norm(g.Quote)
				if win == "" || len(q) < 8 || !strings.Contains(norm(byID[win].Prose), q) {
					continue // no precedence, or one the winner's text does not state
				}
				for _, o := range g.Over {
					if lose := stmtOf(ids, o); lose != "" && lose != win && !slices.Contains(out[win], lose) {
						out[win] = append(out[win], lose)
					}
				}
			}
		}(p, part)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// statementNamed finds which of ids a reply means: by its alias (s2), its
// full ID, or the label its text uses (FR-013), when exactly one matches.
func statementNamed(ids []string, name string) string {
	name = strings.TrimSpace(name)
	var j int
	if n, err := fmt.Sscanf(name, "s%d", &j); err == nil && n == 1 && fmt.Sprintf("s%d", j) == name && j >= 1 && j <= len(ids) {
		return ids[j-1]
	}
	var found []string
	for _, id := range ids {
		_, tail, _ := strings.Cut(id, "/")
		if id == name || tail == name {
			found = append(found, id)
		}
	}
	if len(found) == 1 {
		return found[0]
	}
	return ""
}
