package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/formalize"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// cmdNormalize bundles every attribute a formalized registry's rules use
// and asks the model, in one request over the whole corpus, which of them
// name one fact twice and how the facts relate. Synonyms on one entity
// are merged by renaming them in the rules; relations are added as world
// facts, each kept only if the registry still loads. Requests translated
// in parallel name things independently, so rules about one fact can miss
// each other; this is the corpus-wide view they lack.
func cmdNormalize(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("normalize", flag.ExitOnError)
	regDir := fs.String("registry", "", "formalized registry directory")
	stmtsPath := fs.String("statements", "", "the statements file it was formalized from")
	outDir := fs.String("out", "", "directory for the normalized registry")
	requestTimeout := fs.Duration("request-timeout", 15*time.Minute, "limit for the model call")
	client := llmFlags(fs)
	_ = fs.Parse(args)
	if *regDir == "" || *stmtsPath == "" || *outDir == "" {
		fmt.Fprint(os.Stderr, "usage: stele normalize --registry DIR --statements FILE --out DIR [--llm B]\n")
		return 2
	}
	reg, err := ir.LoadDir(*regDir)
	if err != nil {
		return failf("normalize", err)
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		return failf("normalize", err)
	}
	m, err := client()
	if err != nil {
		return failf("normalize", err)
	}
	prose := map[string]string{}
	for _, s := range stmts {
		prose[s.ID] = strings.Join(strings.Fields(s.Prose), " ")
	}
	// Every attribute a live rule reads, with the statements that read it.
	users := map[*ir.Attr][]string{}
	for _, x := range reg.Rules {
		if !x.Live() {
			continue
		}
		subject, err := reg.SubjectOf(x)
		if err != nil {
			continue
		}
		for _, e := range []ir.Expr{x.When, x.Assert} {
			if e == nil {
				continue
			}
			for _, a := range reg.Reads(e, subject) {
				if a.Entity == nil {
					continue
				}
				if sid := x.Statement(); !slices.Contains(users[a], sid) {
					users[a] = append(users[a], sid)
				}
			}
		}
	}
	var attrs []*ir.Attr
	for a := range users {
		attrs = append(attrs, a)
	}
	sort.Slice(attrs, func(i, j int) bool { return attrs[i].QName() < attrs[j].QName() })
	groups := similarGroups(attrs, 8)
	// Which statements each entity's facts and operations serve.
	entityUsers := map[string][]string{}
	for a, sids := range users {
		for _, sid := range sids {
			if !slices.Contains(entityUsers[a.Entity.Name], sid) {
				entityUsers[a.Entity.Name] = append(entityUsers[a.Entity.Name], sid)
			}
		}
	}
	for _, x := range reg.Rules {
		if op := x.Op(); x.Live() && op != "" && !ir.OpPattern(op) {
			ent, _, _ := strings.Cut(op, ".")
			if sid := x.Statement(); sid != "" && !slices.Contains(entityUsers[ent], sid) {
				entityUsers[ent] = append(entityUsers[ent], sid)
			}
		}
	}
	sameEntities, entityUsage, err := sameEntities(m, reg, entityUsers, prose, *requestTimeout)
	if err != nil {
		return failf("normalize", err)
	}
	system := "Stele describes a software system by a vocabulary and checks requirements over it. The vocabulary:\n\n" +
		translate.RenderVocabulary(reg) + "\n" + ruleLanguage() + normalizeInstructions
	start := time.Now()
	type answer struct {
		Same []struct {
			Keep, Merge, Reason string
		} `json:"same"`
		Facts []struct{ Assert, Reason string } `json:"facts"`
	}
	var got answer
	var usage llm.Usage
	const perRequest = 12
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for lo := 0; lo < len(groups); lo += perRequest {
		part := groups[lo:min(lo+perRequest, len(groups))]
		wg.Add(1)
		go func(part [][]*ir.Attr) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var req strings.Builder
			req.WriteString("Groups of attributes whose names share words, each with up to two statements that use it:\n\n")
			for g, as := range part {
				fmt.Fprintf(&req, "group %d:\n", g+1)
				for _, a := range as {
					fmt.Fprintf(&req, "- %s", a.QName())
					if a.Meaning != "" {
						fmt.Fprintf(&req, " (%s)", a.Meaning)
					}
					req.WriteString("\n")
					for k, sid := range users[a] {
						if k == 2 {
							break
						}
						fmt.Fprintf(&req, "    %s: %s\n", sid, truncate(prose[sid], 200))
					}
				}
				req.WriteString("\n")
			}
			cctx, cancel := context.WithTimeout(context.Background(), *requestTimeout)
			reply, err := m.Complete(cctx, llm.Request{System: system, Turns: []llm.Turn{{Role: "user", Text: req.String()}}, Schema: normalizeSchema})
			cancel()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			usage.Add(reply.Usage)
			var one answer
			if err := json.Unmarshal([]byte(reply.JSON), &one); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			got.Same = append(got.Same, one.Same...)
			got.Facts = append(got.Facts, one.Facts...)
		}(part)
	}
	wg.Wait()
	if firstErr != nil {
		return failf("normalize", firstErr)
	}
	replyJSON, _ := json.Marshal(got)
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return failf("normalize", err)
	}
	if _, err := carryOver(*regDir, *outDir, 0, nil, nil, nil); err != nil {
		return failf("normalize", err)
	}
	// Merge synonyms of one entity and type by renaming in the rules.
	byName := map[string]*ir.Attr{}
	for _, a := range attrs {
		byName[a.QName()] = a
	}
	renames := map[string]map[string]string{} // entity -> from attribute -> to attribute
	var merged []string
	var facts []worldFact
	again := map[string][]string{} // statement -> notes, for synonyms on two entities
	for _, e := range sameEntities {
		for _, sid := range entityUsers[e.Merge] {
			again[sid] = append(again[sid], fmt.Sprintf("review: the entities %s and %s are one thing (%s). Model its facts as attributes of %s, and its operations as operations of %s, adding the attributes or operations it needs there; do not use %s.", e.Merge, e.Keep, e.Reason, e.Keep, e.Keep, e.Merge))
		}
		merged = append(merged, fmt.Sprintf("entity %s onto %s by translating %d statement(s) again (%s)", e.Merge, e.Keep, len(entityUsers[e.Merge]), e.Reason))
	}
	for _, s := range got.Same {
		keep, from := byName[s.Keep], byName[s.Merge]
		if keep == nil || from == nil || keep == from {
			continue
		}
		if keep.Entity != from.Entity || keep.Kind != from.Kind || !sameValues(keep, from) || keep.Optional != from.Optional {
			// Not one symbol in place: the statements that use the other
			// name are translated again onto the kept one.
			for _, sid := range users[from] {
				again[sid] = append(again[sid], fmt.Sprintf("review: %s and %s name one fact (%s). Use %s, and the paths that lead to it, for this fact; do not use %s.", from.QName(), keep.QName(), s.Reason, keep.QName(), from.QName()))
			}
			merged = append(merged, fmt.Sprintf("%s onto %s by translating %d statement(s) again (%s)", from.QName(), keep.QName(), len(users[from]), s.Reason))
			continue
		}
		if renames[keep.Entity.Name] == nil {
			renames[keep.Entity.Name] = map[string]string{}
		}
		renames[keep.Entity.Name][from.Name] = keep.Name
		merged = append(merged, fmt.Sprintf("%s into %s (%s)", from.QName(), keep.QName(), s.Reason))
	}
	if err := renameInRules(*outDir, reg, renames); err != nil {
		return failf("normalize", err)
	}
	for _, f := range got.Facts {
		facts = append(facts, worldFact{Assert: f.Assert, Reason: f.Reason})
	}
	kept, err := addFacts(*outDir, nextFactRound(*outDir), facts)
	if err != nil {
		return failf("normalize", err)
	}
	final := *outDir
	if len(again) > 0 {
		final = filepath.Join(*outDir, "again")
		files, err := carryOver(*outDir, final, 1, again, nil, nil)
		if err != nil {
			return failf("normalize", err)
		}
		var redo []translate.Statement
		for _, st := range stmts {
			if notes, ok := again[st.ID]; ok {
				st.Notes = append(append([]string(nil), st.Notes...), notes...)
				redo = append(redo, st)
			}
		}
		if _, err := formalize.Run(context.Background(), m, redo, final, formalize.Options{VocabFiles: files, Compact: true, Batch: 20, BatchBytes: 12000,
			Parallel: 4, Retrieve: 60, NoConsolidate: true, RequestTimeout: *requestTimeout, Log: io.Discard}); err != nil {
			return failf("normalize", err)
		}
		fmt.Fprintf(out, "translated %d statement(s) again onto the kept names\n", len(redo))
	}
	report, _ := json.MarshalIndent(map[string]any{"backend": m.Name(), "seconds": time.Since(start).Seconds(), "cost_usd": usage.CostUSD + entityUsage.CostUSD, "groups": len(groups), "same_entities": sameEntities,
		"attributes": len(attrs), "merged": merged, "facts_proposed": len(facts), "facts_kept": kept, "reply": json.RawMessage(replyJSON)}, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "normalize.json"), report, 0o644); err != nil {
		return failf("normalize", err)
	}
	fmt.Fprintf(out, "%d attribute(s) used by rules in %d group(s): %d merged as one fact, %d world fact(s) proposed, %d kept; review about $%.2f, %.0fs\nnormalized registry: %s\n",
		len(attrs), len(groups), len(merged), len(facts), len(kept), usage.CostUSD+entityUsage.CostUSD, time.Since(start).Seconds(), final)
	return 0
}

const normalizeInstructions = `
# Your task: normalize the vocabulary

The rules were translated in parallel batches, each naming the facts its
statements needed, so one fact can have several names, and facts that
depend on each other were never linked. Then two requirements about the
same thing never meet, and their contradiction is missed. Read the whole
list and find:

- same: two attributes that name one fact, on one entity or on two entities
  that model the same thing under different names (a warning shown and a
  warning displayed; a cancellation and a withdrawn order; a customer and an
  account holder). Keep the clearer name on the entity the fact belongs to.
  Only when every statement using either means the same fact.
- facts: how facts relate by their nature, as invariants with no operation,
  written in the rule language with entity roots in lower camel case, not
  self: one thing is another under two names on two entities
  (order.paid = order.invoice.paid), a part of another (items
  stored in a warehouse are gone when the warehouse is closed), or implied by
  another (a cancelled order has no open shipment). Only facts true in
  every real situation, by what the things are, never because a requirement
  demands it. Use only the declared attributes and relations.

Never merge or relate two facts to make requirements agree or disagree:
only what the facts are.
`

const normalizeSchema = `{"type":"object","additionalProperties":false,"required":["same","facts"],"properties":{"same":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["keep","merge","reason"],"properties":{"keep":{"type":"string"},"merge":{"type":"string"},"reason":{"type":"string"}}}},"facts":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["assert","reason"],"properties":{"assert":{"type":"string"},"reason":{"type":"string"}}}}}}`

// ruleLanguage is the expression part of the translator's prompt.
func ruleLanguage() string {
	lang := translate.SystemPrompt
	if i := strings.Index(lang, "# Expressions"); i >= 0 {
		if j := strings.Index(lang, "# How to translate"); j > i {
			return lang[i:j]
		}
	}
	return ""
}

func sameValues(a, b *ir.Attr) bool {
	if len(a.Values) != len(b.Values) {
		return false
	}
	for i := range a.Values {
		if a.Values[i] != b.Values[i] {
			return false
		}
	}
	return true
}

// rootPath writes an attribute as a path from its entity's root.
func rootPath(a *ir.Attr) string {
	n := a.Entity.Name
	return strings.ToLower(n[:1]) + n[1:] + "." + a.Name
}

// renameInRules rewrites, in every rule document of dir, the attribute
// names of renames in the "when" and "assert" of rules that read them. A
// rule that also reads another entity's attribute of the same name is left
// as it is, since the name is ambiguous in it.
func renameInRules(dir string, reg *ir.Registry, renames map[string]map[string]string) error {
	if len(renames) == 0 {
		return nil
	}
	ambiguous := func(x *ir.Rule, entity, name string) bool {
		subject, err := reg.SubjectOf(x)
		if err != nil {
			return true
		}
		for _, e := range []ir.Expr{x.When, x.Assert} {
			if e == nil {
				continue
			}
			for _, a := range reg.Reads(e, subject) {
				if a.Name == name && a.Entity != nil && a.Entity.Name != entity {
					return true
				}
			}
		}
		return false
	}
	reads := func(x *ir.Rule, entity, name string) bool {
		subject, err := reg.SubjectOf(x)
		if err != nil {
			return false
		}
		for _, e := range []ir.Expr{x.When, x.Assert} {
			if e == nil {
				continue
			}
			for _, a := range reg.Reads(e, subject) {
				if a.Name == name && a.Entity != nil && a.Entity.Name == entity {
					return true
				}
			}
		}
		return false
	}
	files, err := filepath.Glob(filepath.Join(dir, "*rules.yaml"))
	if err != nil {
		return err
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		docs := strings.Split(string(src), "\n---\n")
		changed := false
		for i, d := range docs {
			var node yaml.Node
			if err := yaml.Unmarshal([]byte(d), &node); err != nil || len(node.Content) == 0 || node.Content[0].Kind != yaml.MappingNode {
				continue
			}
			var doc struct {
				ID string `yaml:"id"`
			}
			if err := node.Decode(&doc); err != nil {
				continue
			}
			x := reg.Rule(doc.ID)
			if x == nil {
				continue
			}
			mp := node.Content[0]
			touched := false
			for entity, names := range renames {
				for from, to := range names {
					if !reads(x, entity, from) || ambiguous(x, entity, from) {
						continue
					}
					seg := regexp.MustCompile(`\.` + regexp.QuoteMeta(from) + `\b`)
					for k := 0; k+1 < len(mp.Content); k += 2 {
						if key := mp.Content[k].Value; key == "when" || key == "assert" {
							if v := mp.Content[k+1]; seg.MatchString(v.Value) {
								v.Value = seg.ReplaceAllString(v.Value, "."+to)
								touched = true
							}
						}
					}
				}
			}
			if !touched {
				continue
			}
			var head string
			if strings.HasPrefix(d, "#") {
				if j := strings.Index(d, "\n"); j >= 0 {
					head = d[:j+1]
				}
			}
			outb, err := yaml.Marshal(&node)
			if err != nil {
				return err
			}
			docs[i] = head + strings.TrimSuffix(string(outb), "\n")
			changed = true
		}
		if changed {
			if err := os.WriteFile(f, []byte(strings.Join(docs, "\n---\n")), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// nameWords splits an attribute's name into lower-case words ("idleShutdownWarningShown"
// -> idle, shutdown, warning, shown), leaving out words too common to link facts.
func nameWords(name string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			w := strings.ToLower(string(cur))
			if len(w) > 2 && !commonWord[w] {
				words = append(words, w)
			}
			cur = nil
		}
	}
	for _, r := range name {
		if unicode.IsUpper(r) || !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
		}
	}
	flush()
	return words
}

var commonWord = map[string]bool{"is": true, "has": true, "the": true, "for": true, "count": true, "status": true, "state": true, "enabled": true, "set": true, "last": true, "self": true}

// similarGroups puts attributes whose names share two words, or one word
// that few attributes use, into groups of at most size, across entities.
func similarGroups(attrs []*ir.Attr, size int) [][]*ir.Attr {
	words := make([][]string, len(attrs))
	df := map[string]int{}
	for i, a := range attrs {
		words[i] = nameWords(a.Name)
		for _, w := range uniqueStrings(words[i]) {
			df[w]++
		}
	}
	parent := make([]int, len(attrs))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	members := func(r int) int {
		n := 0
		for i := range attrs {
			if find(i) == r {
				n++
			}
		}
		return n
	}
	for i := range attrs {
		for j := i + 1; j < len(attrs); j++ {
			shared, rare := 0, false
			for _, w := range uniqueStrings(words[i]) {
				if slices.Contains(words[j], w) {
					shared++
					if df[w] <= 3 {
						rare = true
					}
				}
			}
			if shared < 2 && !rare {
				continue
			}
			ri, rj := find(i), find(j)
			if ri != rj && members(ri)+members(rj) <= size {
				parent[ri] = rj
			}
		}
	}
	byRoot := map[int][]*ir.Attr{}
	for i, a := range attrs {
		r := find(i)
		byRoot[r] = append(byRoot[r], a)
	}
	var out [][]*ir.Attr
	for _, g := range byRoot {
		if len(g) > 1 {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0].QName() < out[j][0].QName() })
	return out
}

func uniqueStrings(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

type entityMerge struct {
	Keep   string `json:"keep"`
	Merge  string `json:"merge"`
	Reason string `json:"reason"`
}

const entityInstructions = `You check a software system's vocabulary for things modelled twice.

The vocabulary was built in parallel batches from requirement statements
written at different times, so one real thing can appear as two entities
under different nouns: a "customer" in one spec and an "account holder" in another for
the same person; a "ticket" and an "issue"; a "task" and a "job".
Then requirements about that thing never meet.

Each item is an entity with its attributes, relations, operations and
statements that use it. List the pairs that are one real thing: every
statement using either means the same thing. Keep the entity the other
statements and relations mostly use. Do not pair a thing with a part of it,
a kind of it, or something it contains, and do not pair two things only
because they are related. When unsure, leave them apart.
`

const entitySchema = `{"type":"object","additionalProperties":false,"required":["same"],"properties":{"same":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["keep","merge","reason"],"properties":{"keep":{"type":"string"},"merge":{"type":"string"},"reason":{"type":"string"}}}}}}`

// sameEntities asks, over every entity rules use, which two model one
// real thing under different nouns.
func sameEntities(m llm.Client, reg *ir.Registry, users map[string][]string, prose map[string]string, timeout time.Duration) ([]entityMerge, llm.Usage, error) {
	var names []string
	for n := range users {
		names = append(names, n)
	}
	sort.Strings(names)
	ops := map[string][]string{}
	for name := range reg.Operations {
		ent, verb, _ := strings.Cut(name, ".")
		ops[ent] = append(ops[ent], verb)
	}
	var req strings.Builder
	for _, n := range names {
		e := reg.Entities[n]
		if e == nil || n == "Request" || n == "Actor" || n == "Result" {
			continue
		}
		var attrs, rels []string
		for a := range e.Attrs {
			attrs = append(attrs, a)
		}
		for r, rel := range e.Relations {
			if strings.HasPrefix(r, "*") {
				continue // a relation the loader adds, not the vocabulary's
			}
			rels = append(rels, r+" -> "+rel.To)
		}
		sort.Strings(attrs)
		sort.Strings(rels)
		sort.Strings(ops[n])
		fmt.Fprintf(&req, "entity %s\n  attributes: %s\n", n, strings.Join(attrs, ", "))
		if len(rels) > 0 {
			fmt.Fprintf(&req, "  relations: %s\n", strings.Join(rels, ", "))
		}
		if len(ops[n]) > 0 {
			fmt.Fprintf(&req, "  operations: %s\n", strings.Join(ops[n], ", "))
		}
		for k, sid := range users[n] {
			if k == 2 {
				break
			}
			fmt.Fprintf(&req, "  %s: %s\n", sid, truncate(prose[sid], 180))
		}
		req.WriteString("\n")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	reply, err := m.Complete(ctx, llm.Request{System: entityInstructions, Turns: []llm.Turn{{Role: "user", Text: req.String()}}, Schema: entitySchema})
	if err != nil {
		return nil, llm.Usage{}, err
	}
	var got struct {
		Same []entityMerge `json:"same"`
	}
	if err := json.Unmarshal([]byte(reply.JSON), &got); err != nil {
		return nil, reply.Usage, err
	}
	var out []entityMerge
	for _, e := range got.Same {
		if reg.Entities[e.Keep] != nil && reg.Entities[e.Merge] != nil && e.Keep != e.Merge {
			out = append(out, e)
		}
	}
	return out, reply.Usage, nil
}
