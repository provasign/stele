package formalize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/provasign/stele/internal/graph"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// compactPass builds only vocabulary needed by the current statements. A
// statement is accepted atomically (all its parts compile), independently of
// its neighbours. Repairs contain only unresolved statements, with the shared
// vocabulary and local diagnostics, not an ever-growing conversation history.
//
// Work runs in waves. Each wave packs every pending statement, repairs
// first, into source-bounded batches and sends up to opt.Parallel of them at
// once, all against the vocabulary as it stood when the wave began. Replies
// are merged in input order, so the result does not depend on which call
// finishes first; a batch whose new vocabulary clashes with an earlier
// batch's is repaired in the next wave against the merged vocabulary.
func compactPass(ctx context.Context, m llm.Client, stmts []translate.Statement, st *state, opt Options, dir string) error {
	batch, byteLimit, repairs, parallel := opt.Batch, opt.BatchBytes, opt.MaxRepairs, opt.Parallel
	if batch <= 0 {
		batch = 20
	}
	if byteLimit <= 0 {
		byteLimit = 12000
	}
	if repairs == 0 {
		repairs = 2
	}
	repairs = max(0, repairs)
	parallel = max(1, parallel)
	schema, err := compactSchema()
	if err != nil {
		return err
	}
	validationDir, err := os.MkdirTemp(dir, ".compact-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(validationDir)
	// The vocabulary the run starts from stays the same through all its
	// waves, and from one check to the next until the registry changes.
	startReg, err := vocab.Load(vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}, vocab.Proposal{})
	if err != nil {
		return err
	}
	base := translate.RenderVocabulary(startReg)
	if st.CompactErrors == nil {
		st.CompactErrors = map[string]string{}
	}
	if st.CompactDeferred == nil {
		st.CompactDeferred = map[string]string{}
	}
	accepted := map[string]bool{}
	order := map[string]int{}
	for i, s := range stmts {
		order[s.ID] = i
	}
	for _, c := range st.Cands {
		accepted[c.StatementID] = true
	}
	attempts := map[string]int{} // requests that included the statement, this session
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var pending []translate.Statement
		deferred := false
		for _, s := range stmts {
			if _, done := st.CompactDeferred[s.ID]; accepted[s.ID] || done {
				continue
			}
			if attempts[s.ID] > repairs {
				st.CompactDeferred[s.ID] = st.CompactErrors[s.ID]
				fmt.Fprintf(opt.Log, "  compact: deferred %s for review after %d repair(s): %s\n", s.ID, repairs, st.CompactErrors[s.ID])
				deferred = true
				continue // later documents must not wait on untranslatable statements
			}
			pending = append(pending, s)
		}
		if deferred {
			if err := saveState(dir, st); err != nil {
				return err
			}
		}
		if len(pending) == 0 {
			return nil
		}
		// Repairs go first, so they are never starved by fresh statements.
		sort.SliceStable(pending, func(i, j int) bool {
			ri, rj := st.CompactErrors[pending[i].ID] != "", st.CompactErrors[pending[j].ID] != ""
			return ri && !rj
		})
		parts := packCompact(pending, batch, byteLimit, parallel, opt.AcrossDocuments)
		cur, err := vocab.Load(vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}, st.Proposal)
		if err != nil {
			return err
		}
		shared := translate.RenderVocabulary(cur)
		var g *graph.Graph
		if opt.Retrieve > 0 {
			g = graph.Build(cur)
		}
		type result struct {
			reply   *llm.Reply
			aliases map[string]string
			err     error
		}
		results := make([]result, len(parts))
		// The shared vocabulary and the code's structure are the same in
		// every request of the wave, so they go in the system prompt, which
		// the backend caches: a warm-up call writes it once and the wave's
		// requests read it (only an identical system prompt is read from the
		// cache; the same text at the start of the request is written again,
		// measured 2026-11-06). A vocabulary retrieved per request goes in it.
		// What this run added goes in the request instead when that costs
		// less: the system prompt then stays the one earlier waves and checks
		// wrote, and only the additions are paid for again in each request.
		// A two-requirement check sent a 15,000-token prompt five times, its
		// new terms changing it every wave, and read none of it from the
		// cache (2026-11-07).
		system, added := compactPrompt, ""
		if g == nil {
			added = vocabularyDelta(base, shared)
			if len(parts)*len(added) > len(compactPrompt)+len(shared) {
				base, added = shared, "" // later waves add to what this one caches
			}
			system += "\n\n# Shared vocabulary\n\n" + base + codeContext(opt.CodeContext)
		}
		if r := llm.Warm(ctx, m, len(parts), llm.Request{System: system, Turns: []llm.Turn{{Role: "user", Text: warmCompact}}, Schema: schema}); r != nil {
			st.Usage.Add(r.Usage)
		}
		var wg sync.WaitGroup
		for i, part := range parts {
			aliases := map[string]string{}
			var req strings.Builder
			if g != nil {
				vocabulary := translate.RenderVocabulary(g.Subset(retrieved(g, part, st.Proposal, opt.Retrieve)))
				req.WriteString("# Shared vocabulary\n\n" + vocabulary + codeContext(opt.CodeContext) + "\n")
			}
			if added != "" {
				req.WriteString("# Vocabulary added in this run\n\nThese entries are part of the shared vocabulary; where one names an entity, attribute or operation listed above, it replaces that line.\n\n" + added + "\n")
			}
			req.WriteString("# Statements\n\n")
			req.WriteString("Translate only these statements. Propose only vocabulary they need beyond the shared vocabulary.\n\n")
			for k, s := range part {
				attempts[s.ID]++
				alias := fmt.Sprintf("s%d", k+1)
				aliases[alias] = s.ID
				fmt.Fprintf(&req, "statement_id: %s\nsource: %s\ntext: %s\n", alias, s.Source, s.Prose)
				for _, note := range s.Notes {
					if r, ok := strings.CutPrefix(note, "review: "); ok {
						fmt.Fprintf(&req, "review note: %s\n", r)
						continue
					}
					fmt.Fprintf(&req, "vocabulary note: %s\n", note)
				}
				if issue := st.CompactErrors[s.ID]; issue != "" && !strings.HasPrefix(issue, backendFailed) {
					issue = strings.ReplaceAll(issue, s.ID, alias)
					fmt.Fprintf(&req, "Previous attempt failed: %s\nReturn every part of this statement, fixing the error.\n", issue)
				}
				req.WriteString("\n")
			}
			results[i].aliases = aliases
			wg.Add(1)
			go func(i int, text string) {
				defer wg.Done()
				results[i].reply, results[i].err = m.Complete(ctx, llm.Request{System: system, Turns: []llm.Turn{{Role: "user", Text: text}}, Schema: schema})
			}(i, req.String())
		}
		fmt.Fprintf(opt.Log, "  compact: %d statement(s) in %d request(s), %d already accepted\n", len(pending), len(parts), len(accepted))
		wg.Wait()
		var firstErr error
		for i, part := range parts {
			r := results[i]
			if r.err == nil && r.reply == nil {
				r.err = fmt.Errorf("backend returned no reply")
			}
			if r.err != nil {
				// Accepted work from the other calls is still merged and saved.
				// A call that failed on its own (its deadline, a crashed CLI)
				// is a failed attempt for its statements, retried in the next
				// wave and deferred after the repair budget; cancellation, the
				// run's deadline and the call budget stop the run.
				if j, ok := m.(*callJournal); (ok && j.stopErr() != nil) || ctx.Err() != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("compact: %w", r.err)
					}
					continue
				}
				stalled := errors.Is(r.err, context.DeadlineExceeded)
				fmt.Fprintf(opt.Log, "  compact: request for %d statement(s) failed, retrying them: %v\n", len(part), r.err)
				for _, s := range part {
					if _, rereading := st.Rereading[s.ID]; stalled && rereading {
						// A reread that runs to the request limit is not sent
						// again: it keeps the earlier reading, as a failed
						// reread does. On Codex the same reread stalled twice
						// for 280 seconds each and then gave no rules (a
						// developer trial, 2026-11-06).
						attempts[s.ID] = repairs + 1
					}
					if st.CompactErrors[s.ID] == "" || strings.HasPrefix(st.CompactErrors[s.ID], backendFailed) {
						st.CompactErrors[s.ID] = backendFailed + r.err.Error()
					}
				}
				continue
			}
			st.Usage.Add(r.reply.Usage)
			if thinReply(r.reply.JSON, len(part)) {
				// A reply cut short still parses: most statements come back
				// informal with a word or two (ISSUES 44). Retry them in
				// smaller requests.
				batch = max(1, min(batch, len(part))/2)
				fmt.Fprintf(opt.Log, "  compact: reply for %d statement(s) looks cut short; retrying in requests of %d\n", len(part), batch)
				for _, s := range part {
					if st.CompactErrors[s.ID] == "" || strings.HasPrefix(st.CompactErrors[s.ID], backendFailed) {
						st.CompactErrors[s.ID] = backendFailed + "reply looked cut short"
					}
				}
				continue
			}
			if err := mergeCompact(st, part, r.reply.JSON, r.aliases, accepted, opt, validationDir); err != nil {
				return err
			}
		}
		sort.SliceStable(st.Cands, func(i, j int) bool { return order[st.Cands[i].StatementID] < order[st.Cands[j].StatementID] })
		if err := saveState(dir, st); err != nil {
			return err
		}
		if firstErr != nil {
			return firstErr
		}
	}
}

// vocabularyDelta is what cur, a rendered vocabulary, says that base does
// not: each line base lacks, under its entity or section heading. A line
// that changed (an attribute a new operation writes) is in it whole.
func vocabularyDelta(base, cur string) string {
	have := map[string]bool{}
	for _, l := range strings.Split(base, "\n") {
		have[l] = true
	}
	var b strings.Builder
	heading, shown := "", false
	for _, l := range strings.Split(cur, "\n") {
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, " ") {
			heading, shown = l, false
			if have[l] {
				continue
			}
		} else if have[l] {
			continue
		}
		if !shown {
			b.WriteString(heading + "\n")
			shown = true
		}
		if l != heading {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// warmCompact is the warm-up request of a translation wave (llm.Warm).
const warmCompact = "This request has no statements: translate nothing, and return every list empty."

// thinReply reports a reply for n statements (at least 8) in which most
// statements are only informal, each with a reason of three words or fewer:
// what a reply cut short by the backend's output limit looks like.
func thinReply(js string, n int) bool {
	if n < 8 {
		return false
	}
	var out struct {
		Rules []struct {
			ID        string `json:"statement_id"`
			Kind      string `json:"kind"`
			Rationale string `json:"rationale"`
		} `json:"rules"`
	}
	js, err := flatCompact(js)
	if err != nil || json.Unmarshal([]byte(js), &out) != nil {
		return false
	}
	thin := map[string]bool{}
	for _, r := range out.Rules {
		ok, seen := thin[r.ID]
		thin[r.ID] = (ok || !seen) && r.Kind == "informal" && len(strings.Fields(r.Rationale)) <= 3
	}
	k := 0
	for _, t := range thin {
		if t {
			k++
		}
	}
	return k*5 >= n*3
}

// retrieved is the vocabulary a request shows: the k existing attributes
// most related to its statements, and every attribute this run proposed.
func retrieved(g *graph.Graph, part []translate.Statement, p vocab.Proposal, k int) []*ir.Attr {
	var text strings.Builder
	for _, s := range part {
		text.WriteString(s.Prose + "\n")
	}
	out := g.Related(text.String(), k)
	for _, es := range [][]vocab.Entity{p.Entities, p.Extensions} {
		for _, e := range es {
			for _, a := range e.Attributes {
				name, field, _ := strings.Cut(a.Name, ".")
				ent := g.Reg.Entities[e.Name]
				if field != "" {
					ent = g.Reg.Entities[e.Name+"$"+name]
					name = field
				}
				if ent != nil && ent.Attrs[name] != nil && !slices.Contains(out, ent.Attrs[name]) {
					out = append(out, ent.Attrs[name])
				}
			}
		}
	}
	return out
}

// backendFailed marks a statement whose last request failed in the backend,
// not in its translation; the reason is not shown to the model.
const backendFailed = "backend: "

// packCompact splits pending statements into at most n batches of at most
// size statements and limit source bytes, each from one document unless
// across is set. A single oversized statement stays intact. Statements that
// do not fit wait for the next wave.
func packCompact(pending []translate.Statement, size, limit, n int, across bool) [][]translate.Statement {
	var parts [][]translate.Statement
	open := map[string]int{} // document -> index of its batch with room
	bytes := map[int]int{}
	for _, s := range pending {
		doc, _, _ := strings.Cut(s.Source, "#")
		if across {
			doc = ""
		}
		sz := len(s.Prose) + len(s.Source) + len(strings.Join(s.Notes, "\n"))
		if i, ok := open[doc]; ok && len(parts[i]) < size && bytes[i]+sz <= limit {
			parts[i] = append(parts[i], s)
			bytes[i] += sz
			continue
		}
		if len(parts) == n {
			continue
		}
		open[doc] = len(parts)
		bytes[len(parts)] = sz
		parts = append(parts, []translate.Statement{s})
	}
	return parts
}

// mergeCompact accepts the statements of one reply whose rules all compile
// against the vocabulary merged so far, and records why the others failed.
func mergeCompact(st *state, part []translate.Statement, reply string, aliases map[string]string, accepted map[string]bool, opt Options, validationDir string) error {
	out, decodeErr := decodeCompact(reply, aliases)
	var problem string
	if decodeErr != nil {
		problem = "reply format: " + decodeErr.Error()
	} else if err := uniqueCompactMembers(out.Proposal); err != nil {
		problem = "vocabulary: " + err.Error()
	} else {
		// Fold the reply's vocabulary into ours member by member: replies
		// written in parallel declare the same entities and operations.
		known, err := vocab.Load(vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}, st.Proposal)
		if err != nil {
			return err
		}
		p, clashes := foldCompact(st.Proposal, out.Proposal, known)
		vopt := vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}
		if _, err := vocab.Load(vopt, p); err != nil {
			// Keep what loads: one bad member must not reject the reply.
			var dropped []string
			p, dropped = salvage(st.Proposal, out.Proposal, known, vopt)
			fmt.Fprintf(opt.Log, "  compact: vocabulary did not load (%v); kept the rest without %s\n", oneLine(err.Error(), 200), strings.Join(dropped, ", "))
			clashes = append(clashes, dropped...)
		}
		if _, err := vocab.Load(vopt, p); err != nil {
			problem = "vocabulary: " + err.Error()
		} else {
			files, err := writeVocabulary(validationDir, p, opt)
			if err != nil {
				return err
			}
			assign(out.Rules)
			translate.Revalidate(files, out.Rules)
			byID := map[string][]translate.Candidate{}
			for _, c := range out.Rules {
				byID[c.StatementID] = append(byID[c.StatementID], c)
			}
			st.Proposal = p
			for _, s := range part {
				cs := byID[s.ID]
				var issues []string
				if len(cs) == 0 {
					issues = append(issues, "no entry for this statement")
				}
				for _, c := range cs {
					if c.Error != "" {
						issues = append(issues, fmt.Sprintf("%s: %s (when %q; assert %q)", c.Kind, c.Error, c.When, c.Assert))
					}
				}
				if len(issues) > 0 {
					if len(clashes) > 0 {
						issues = append(issues, "the shared vocabulary kept its definitions of "+strings.Join(clashes, ", ")+"; use them as declared")
					}
					st.CompactErrors[s.ID] = strings.Join(issues, "; ")
					continue
				}
				st.Cands = append(st.Cands, cs...)
				accepted[s.ID] = true
				delete(st.CompactErrors, s.ID)
			}
		}
	}
	if problem != "" {
		for _, s := range part {
			st.CompactErrors[s.ID] = problem
		}
	}
	return nil
}

// The vocabulary serializer uses maps for attributes and relations. Reject
// repeated member names before serialization can silently overwrite one.
func uniqueCompactMembers(p vocab.Proposal) error {
	for _, es := range [][]vocab.Entity{p.Entities, p.Extensions} {
		for _, e := range es {
			seen := map[string]bool{}
			for _, a := range e.Attributes {
				if seen[a.Name] {
					return fmt.Errorf("duplicate attribute %s.%s", e.Name, a.Name)
				}
				seen[a.Name] = true
			}
			for _, r := range e.Relations {
				if seen[r.Name] {
					return fmt.Errorf("duplicate member %s.%s", e.Name, r.Name)
				}
				seen[r.Name] = true
			}
		}
	}
	return nil
}

// foldCompact adds a reply's vocabulary to base. An entity or operation
// already declared (in base or the vocabulary files) gains the reply's new
// members: attributes, relations and writes. Replies written in parallel see
// different parts of one attribute, so an enum's values and an int's range
// are widened to cover both. A member the reply defines differently in kind
// (another type, controller, relation target or operation default) keeps
// base's definition and is returned as a clash; rules written against the
// other definition then fail to compile and only their statements are
// repaired. Writes of attributes the world sets, and of the request and
// actor (inputs, not state), are dropped, as the compiler requires. Similar names are never treated as one.
func foldCompact(base, reply vocab.Proposal, known *ir.Registry) (vocab.Proposal, []string) {
	out := vocab.Proposal{
		Entities:   slices.Clone(base.Entities),
		Extensions: slices.Clone(base.Extensions),
		Operations: slices.Clone(base.Operations),
	}
	var clashes []string
	// where finds the proposal entry holding an entity's members.
	where := func(name string) *vocab.Entity {
		for _, es := range []*[]vocab.Entity{&out.Entities, &out.Extensions} {
			for i := range *es {
				if (*es)[i].Name == name {
					(*es)[i].Attributes = slices.Clone((*es)[i].Attributes)
					(*es)[i].Relations = slices.Clone((*es)[i].Relations)
					return &(*es)[i]
				}
			}
		}
		return nil
	}
	fold := func(e vocab.Entity, extension bool) {
		have := where(e.Name)
		declared := known != nil && known.Entities[e.Name] != nil
		if have == nil && !declared {
			if extension {
				out.Extensions = append(out.Extensions, e)
			} else {
				out.Entities = append(out.Entities, e)
			}
			return
		}
		if have == nil {
			// Declared only in the vocabulary files: extend it.
			out.Extensions = append(out.Extensions, vocab.Entity{Name: e.Name})
			have = &out.Extensions[len(out.Extensions)-1]
		}
		for _, a := range e.Attributes {
			q := e.Name + "." + a.Name
			if i := slices.IndexFunc(have.Attributes, func(b vocab.Attribute) bool { return b.Name == a.Name }); i >= 0 {
				if wide, ok := widen(have.Attributes[i], a); ok {
					have.Attributes[i] = wide
				} else {
					clashes = append(clashes, q)
				}
				continue
			}
			if declared && known.Entities[e.Name].Attrs[a.Name] != nil {
				continue // declared in the vocabulary files; theirs stands
			}
			have.Attributes = append(have.Attributes, a)
		}
		for _, r := range e.Relations {
			if i := slices.IndexFunc(have.Relations, func(b vocab.Relation) bool { return b.Name == r.Name }); i >= 0 {
				if have.Relations[i].To != r.To || have.Relations[i].Many != r.Many || have.Relations[i].Optional != r.Optional {
					clashes = append(clashes, e.Name+"."+r.Name)
				}
				continue
			}
			if declared && known.Entities[e.Name].Relations[r.Name] != nil {
				continue
			}
			have.Relations = append(have.Relations, r)
		}
		have.StatementIDs = union(have.StatementIDs, e.StatementIDs)
	}
	for _, e := range reply.Entities {
		fold(e, false)
	}
	for _, e := range reply.Extensions {
		fold(e, true)
	}
	world := map[string]bool{}
	for _, es := range [][]vocab.Entity{out.Entities, out.Extensions} {
		for _, e := range es {
			for _, a := range e.Attributes {
				if a.SetBy == "world" {
					world[e.Name+"."+a.Name] = true
				}
			}
		}
	}
	if known != nil {
		for _, e := range known.Entities {
			for _, a := range e.Attrs {
				if a.WorldSet {
					world[e.Name+"."+a.Name] = true
				}
			}
		}
	}
	for _, o := range reply.Operations {
		o.Writes = slices.DeleteFunc(slices.Clone(o.Writes), func(w string) bool {
			ent, _, _ := strings.Cut(w, ".")
			return world[w] || ent == "Request" || ent == "Actor" || ent == "Result" // inputs and results, not state
		})
		i := slices.IndexFunc(out.Operations, func(b vocab.Operation) bool { return b.Name == o.Name })
		switch {
		case i < 0 && (known == nil || known.Operations[o.Name] == nil):
			out.Operations = append(out.Operations, o)
		case i < 0:
			// Declared in the vocabulary files; theirs stands.
		case out.Operations[i].Default != o.Default:
			clashes = append(clashes, o.Name)
		default:
			// An operation writes everything any reply says it changes.
			out.Operations[i].Writes = union(out.Operations[i].Writes, o.Writes)
			out.Operations[i].StatementIDs = union(out.Operations[i].StatementIDs, o.StatementIDs)
		}
	}
	return out, clashes
}

// salvage folds a reply's vocabulary into base one member at a time and
// keeps each member only if the vocabulary still loads: entities and
// extensions whole when they can, else attribute by attribute, then
// operations, dropping writes of attributes that did not survive. It
// returns the result and the members it dropped. Rules that need a dropped
// member then fail to compile, and only their statements are repaired.
func salvage(base, reply vocab.Proposal, known *ir.Registry, vopt vocab.Options) (vocab.Proposal, []string) {
	cur := base
	var dropped []string
	loads := func(p vocab.Proposal) bool { _, err := vocab.Load(vopt, p); return err == nil }
	try := func(add vocab.Proposal) bool {
		next, _ := foldCompact(cur, add, known)
		if loads(next) {
			cur = next
			return true
		}
		return false
	}
	for _, group := range []struct {
		es        []vocab.Entity
		extension bool
	}{{reply.Entities, false}, {reply.Extensions, true}} {
		for _, e := range group.es {
			wrap := func(x vocab.Entity) vocab.Proposal {
				if group.extension {
					return vocab.Proposal{Extensions: []vocab.Entity{x}}
				}
				return vocab.Proposal{Entities: []vocab.Entity{x}}
			}
			if try(wrap(e)) {
				continue
			}
			// Member by member: the entity first, then each attribute and relation.
			shell := e
			shell.Attributes, shell.Relations = nil, nil
			if !try(wrap(shell)) {
				dropped = append(dropped, e.Name)
				continue
			}
			for _, a := range e.Attributes {
				one := shell
				one.Attributes = []vocab.Attribute{a}
				if !try(wrap(one)) {
					dropped = append(dropped, e.Name+"."+a.Name)
				}
			}
			for _, r := range e.Relations {
				one := shell
				one.Relations = []vocab.Relation{r}
				if !try(wrap(one)) {
					dropped = append(dropped, e.Name+"."+r.Name)
				}
			}
		}
	}
	for _, o := range reply.Operations {
		if try(vocab.Proposal{Operations: []vocab.Operation{o}}) {
			continue
		}
		var keep []string
		for _, w := range o.Writes {
			one := o
			one.Writes = append(append([]string{}, keep...), w)
			if next, _ := foldCompact(cur, vocab.Proposal{Operations: []vocab.Operation{one}}, known); loads(next) {
				keep = append(keep, w)
			} else {
				dropped = append(dropped, o.Name+" writes "+w)
			}
		}
		o.Writes = keep
		if !try(vocab.Proposal{Operations: []vocab.Operation{o}}) {
			dropped = append(dropped, o.Name)
		}
	}
	return cur, dropped
}

// widen combines two definitions of one attribute, ignoring wording: enum
// values and int or decimal ranges are unioned. It fails for a different
// type or controller.
func widen(a, b vocab.Attribute) (vocab.Attribute, bool) {
	norm := func(t string) string { return strings.ToLower(strings.ReplaceAll(t, " ", "")) }
	if norm(a.Type) != norm(b.Type) || (a.SetBy == "world") != (b.SetBy == "world") {
		return a, false
	}
	// Enum values (of an enum or a collection of them) and declared ranges.
	if len(a.Values) > 0 || len(b.Values) > 0 {
		a.Values = union(a.Values, b.Values)
	}
	if a.Min != a.Max || b.Min != b.Max {
		switch {
		case a.Min == a.Max:
			a.Min, a.Max = b.Min, b.Max
		case b.Min != b.Max:
			a.Min, a.Max = min(a.Min, b.Min), max(a.Max, b.Max)
		}
	}
	return a, true
}

func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// codeContext renders the structure of the project's code (D-053) after
// the reply contract, so the vocabulary follows the code's types and steps.
func codeContext(text string) string {
	if text == "" {
		return ""
	}
	return `
# The system as its code defines it

The structure below was extracted from the project's code. Build the
vocabulary on it:
- Name entities, attributes and operations after the code's types and
  functions.
- An enum is one attribute whose values are its variants, so they exclude
  each other: an error enum is the outcome of the step that returns it.
- A record's fields are attributes; an optional field (Option, a pointer,
  nullable) is optional.
- A function's calls are its steps, in order. A step that returns an error
  stops the operation, so a statement about a later step applies only where
  the earlier steps passed: put that in its "when".

The spec statements remain the requirements: translate what each says, even
where the code differs from it, and never use the code to narrow or drop
part of a statement.

` + text + "\n"
}
