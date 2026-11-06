// Package vocab proposes vocabulary from spec prose, for projects that have
// no schema or API definition to infer it from (docs/07 model inference).
//
// An LLM proposes the entities, attributes, relations and operations the
// statements need, given the vocabulary so far. Deterministic code then
// checks the proposal: it must compile with the existing vocabulary (errors
// go back to the model for repair), and every new symbol is compared with
// the existing ones and flagged when it may duplicate one, which docs/07
// names the main risk to cross-time detection. The result is a draft for
// the owner to review; nothing is merged automatically.
//
// Batches run in order, each seeing what earlier batches proposed, so later
// specs reuse earlier terms instead of inventing parallel ones.
package vocab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// Attribute is a proposed attribute.
type Attribute struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"` // bool | int | enum
	Values []string `json:"values"`
	Min    int      `json:"min"`
	Max    int      `json:"max"`
	// Meaning is the fact in plain words; set for attributes from facts.
	Meaning string `json:"meaning,omitempty"`
	// SetBy is "world" for a value the world decides; an operation may only
	// reset it to a fixed value, through its Effect.
	SetBy string `json:"set_by,omitempty"`
}

// Relation is a proposed relation.
type Relation struct {
	Name     string `json:"name"`
	To       string `json:"to"`
	Many     bool   `json:"many,omitempty"`     // to several instances
	Optional bool   `json:"optional,omitempty"` // may lead nowhere
}

// Entity is a proposed new entity, or (as an extension) attributes and
// relations added to an existing one.
type Entity struct {
	Name         string      `json:"name"`
	StatementIDs []string    `json:"statement_ids"`
	Attributes   []Attribute `json:"attributes"`
	Relations    []Relation  `json:"relations"`
}

// Operation is a proposed operation.
type Operation struct {
	Name         string   `json:"name"` // Entity.op
	StatementIDs []string `json:"statement_ids"`
	Default      string   `json:"default"`
	Writes       []string `json:"writes"`
	Rationale    string   `json:"rationale"`
	// Effect resets values the world sets to a fixed value, by attribute
	// name on the operation's entity (ISSUES 19).
	Effect map[string]string `json:"effect,omitempty"`
}

// Proposal is the vocabulary a run proposes.
type Proposal struct {
	Entities   []Entity    `json:"entities"`
	Extensions []Entity    `json:"extensions"`
	Operations []Operation `json:"operations"`
}

// Flag marks a proposed symbol that may duplicate an existing one.
type Flag struct {
	Symbol  string `json:"symbol"`
	Similar string `json:"similar_to"`
	Reason  string `json:"reason"`
}

// Report is a run's record.
type Report struct {
	Backend  string    `json:"backend"`
	Batches  int       `json:"batches"`
	Rounds   int       `json:"rounds"`
	Usage    llm.Usage `json:"usage"`
	Flags    []Flag    `json:"possible_duplicates"`
	Errors   []string  `json:"errors,omitempty"` // batches whose proposal never compiled
	Facts    []Fact    `json:"facts,omitempty"`
	Merges   []Merge   `json:"merges,omitempty"`
	Proposal Proposal  `json:"proposal"`
}

// Options control a run.
type Options struct {
	VocabFiles []string // existing vocabulary; may be just a context file
	Context    string   // context new symbols belong to
	BatchSize  int
	MaxRepairs int
	// Facts finds the facts across all statements first and names one
	// attribute per fact; the batches then add operations and relations.
	Facts     bool
	FactBatch int // statements per facts request
	// Reconsolidate asks once more, after the batches, which attributes of
	// the whole proposal are one fact under two names, and merges them:
	// batches add attributes the facts step never saw.
	Reconsolidate bool
	Log           io.Writer // progress, one line per request; nil for none
}

func (o Options) logf(format string, args ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format+"\n", args...)
	}
}

// Propose proposes vocabulary for stmts, batch by batch.
func Propose(ctx context.Context, m llm.Client, stmts []translate.Statement, opt Options) (*Report, error) {
	if opt.BatchSize <= 0 {
		opt.BatchSize = 40
	}
	if opt.MaxRepairs == 0 {
		opt.MaxRepairs = 2
	}
	if opt.Context == "" {
		opt.Context = "main"
	}
	base, err := ir.LoadFiles(opt.VocabFiles...)
	if err != nil {
		return nil, fmt.Errorf("vocabulary: %w", err)
	}
	if opt.FactBatch <= 0 {
		opt.FactBatch = 150
	}
	rep := &Report{Backend: m.Name()}
	var acc Proposal
	if opt.Facts {
		facts, err := findFacts(ctx, m, stmts, base, opt, rep)
		if err != nil {
			return nil, err
		}
		rep.Facts = facts
		acc = factsProposal(base, facts)
	}
	for i := 0; i < len(stmts); i += opt.BatchSize {
		batch := stmts[i:min(i+opt.BatchSize, len(stmts))]
		rep.Batches++
		opt.logf("  vocabulary batch %d: statements %d-%d", rep.Batches, i+1, i+len(batch))
		p, err := proposeBatch(ctx, m, batch, &acc, opt, rep)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("batch %d (%s..%s): %v", rep.Batches, batch[0].ID, batch[len(batch)-1].ID, err))
			continue
		}
		acc = merge(acc, p)
	}
	if opt.Reconsolidate {
		merged, merges, err := reconsolidate(ctx, m, base, acc, rep)
		if err != nil {
			rep.Errors = append(rep.Errors, "reconsolidation: "+err.Error())
		} else if _, lerr := loadWith(opt, merged); lerr != nil {
			rep.Errors = append(rep.Errors, "reconsolidation skipped: "+lerr.Error())
		} else {
			acc = merged
			rep.Merges = append(rep.Merges, merges...)
		}
	}
	rep.Proposal = acc
	rep.Flags = duplicates(base, acc)
	return rep, nil
}

func proposeBatch(ctx context.Context, m llm.Client, batch []translate.Statement, acc *Proposal, opt Options, rep *Report) (Proposal, error) {
	current, err := loadWith(opt, *acc)
	if err != nil {
		return Proposal{}, fmt.Errorf("vocabulary so far does not load: %w", err)
	}
	system := systemPrompt + translate.RenderVocabulary(current)
	var req strings.Builder
	if opt.Facts {
		req.WriteString("The attributes above were found as the facts all statements talk about; each has a meaning. Propose the operations and relations these statements need. Propose a new attribute only when no fact covers a statement, never a second name for a fact.\n")
	}
	req.WriteString("Propose the vocabulary these statements need, beyond the vocabulary above. Under each statement are existing symbols whose names share its words: reuse them when they cover it.\n\n")
	for _, s := range batch {
		fmt.Fprintf(&req, "statement_id: %s\nsource: %s\ntext: %s\n", s.ID, s.Source, s.Prose)
		if rel := translate.Related(current, s.Prose, 12); len(rel) > 0 {
			fmt.Fprintf(&req, "related symbols: %s\n", strings.Join(rel, ", "))
		}
		req.WriteString("\n")
	}
	turns := []llm.Turn{{Role: "user", Text: req.String()}}
	for round := 0; ; round++ {
		reply, err := m.Complete(ctx, llm.Request{System: system, Turns: turns, Schema: outputSchema})
		if err != nil {
			return Proposal{}, err
		}
		rep.Rounds++
		rep.Usage.Add(reply.Usage)
		var p Proposal
		if err := json.Unmarshal([]byte(reply.JSON), &p); err != nil {
			return Proposal{}, fmt.Errorf("reply is not the expected JSON: %w", err)
		}
		_, lerr := loadWith(opt, merge(*acc, p))
		if lerr == nil {
			return p, nil
		}
		if round >= opt.MaxRepairs {
			return Proposal{}, lerr
		}
		turns = append(turns, llm.Turn{Role: "assistant", Text: reply.JSON}, llm.Turn{Role: "user", Text: "The proposal does not compile with the vocabulary:\n" + lerr.Error() +
			"\n\nReturn the complete corrected proposal for this batch. Reuse existing symbols instead of redeclaring them; add to an existing entity through extensions."})
	}
}

func merge(a, b Proposal) Proposal {
	return Proposal{
		Entities:   append(append([]Entity{}, a.Entities...), b.Entities...),
		Extensions: append(append([]Entity{}, a.Extensions...), b.Extensions...),
		Operations: append(append([]Operation{}, a.Operations...), b.Operations...),
	}
}

// loadWith loads the vocabulary together with a proposal.
func loadWith(opt Options, p Proposal) (*ir.Registry, error) {
	src, err := YAML(p, opt.Context)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "stele-vocab-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "proposed.yaml")
	if err := os.WriteFile(f, src, 0o644); err != nil {
		return nil, err
	}
	reg, err := ir.LoadFiles(append(append([]string{}, opt.VocabFiles...), f)...)
	if err != nil {
		return nil, fmt.Errorf("%s", strings.ReplaceAll(err.Error(), f+":", "proposal:"))
	}
	return reg, nil
}

// YAML renders a proposal as registry documents.
func YAML(p Proposal, context string) ([]byte, error) {
	var docs []map[string]any
	attrs := func(as []Attribute) map[string]any {
		m := map[string]any{}
		for _, a := range as {
			d := map[string]any{"type": a.Type}
			if a.Meaning != "" {
				d["meaning"] = a.Meaning
			}
			if a.SetBy == "world" {
				d["setBy"] = "world"
			}
			// Values for an enum (or a collection of enums), a range for an
			// int or decimal (or a collection of them) that declares one.
			if len(a.Values) > 0 {
				d["values"] = a.Values
			}
			if t := strings.ToLower(a.Type); a.Min != a.Max && (strings.Contains(t, "int") || strings.Contains(t, "decimal")) {
				d["range"] = []int{a.Min, a.Max}
			} else if t == "int" && a.Min == a.Max && a.Min != 0 {
				d["range"] = []int{a.Min, a.Max}
			}
			m[a.Name] = d
		}
		return m
	}
	rels := func(rs []Relation) map[string]any {
		m := map[string]any{}
		for _, r := range rs {
			d := map[string]any{"to": r.To}
			if r.Many {
				d["many"] = true
			}
			// A relation to many may be empty already, so optional says
			// nothing more; a proposal that sets both is not sent back for
			// it (a held-out run, 2026-11-06, spent two repair calls on it).
			if r.Optional && !r.Many {
				d["optional"] = true
			}
			m[r.Name] = d
		}
		return m
	}
	add := func(kind string, e Entity) {
		d := map[string]any{kind: e.Name, "context": context}
		if len(e.Attributes) > 0 {
			d["attributes"] = attrs(e.Attributes)
		}
		if len(e.Relations) > 0 {
			d["relations"] = rels(e.Relations)
		}
		docs = append(docs, d)
	}
	for _, e := range p.Entities {
		add("entity", e)
	}
	for _, e := range p.Extensions {
		add("extend", e)
	}
	for _, o := range p.Operations {
		writes := o.Writes
		if writes == nil {
			writes = []string{}
		}
		d := map[string]any{"operation": o.Name, "context": context, "default": o.Default, "writes": writes}
		if len(o.Effect) > 0 {
			d["effect"] = o.Effect
		}
		docs = append(docs, d)
	}
	if len(docs) == 0 {
		return nil, nil
	}
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// --- duplicate detection -----------------------------------------------------

// norm folds case, drops separators and a plural s, so VM, Vm and vms match.
func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return strings.TrimSuffix(b.String(), "s")
}

// similar reports whether two names are the same after normalising, or one edit apart.
func similar(a, b string) bool {
	a, b = norm(a), norm(b)
	if a == b {
		return true
	}
	if len(a) < 4 || len(b) < 4 { // short names differ by one letter legitimately
		return false
	}
	return editDistance(a, b) <= 1
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

type symbol struct {
	name   string // qualified
	short  string
	owner  string // entity, for attributes and operations
	values string // sorted enum values
	writes string // sorted writes, for operations
}

// duplicates flags each proposed symbol that may duplicate an existing or an
// earlier proposed one: a near-identical name, an enum with the same values
// on the same entity, or an operation writing exactly the same attributes.
func duplicates(base *ir.Registry, p Proposal) []Flag {
	var known []symbol
	for _, e := range base.Entities {
		known = append(known, symbol{name: e.Name, short: e.Name})
		for _, a := range e.Attrs {
			known = append(known, attrSymbol(e.Name, a.Name, a.Values))
		}
	}
	for _, o := range base.Operations {
		var ws []string
		for _, a := range o.Writes {
			ws = append(ws, a.QName())
		}
		known = append(known, opSymbol(o.Name, ws))
	}
	var flags []Flag
	check := func(s symbol, kind string) {
		for _, k := range known {
			if k.name == s.name {
				continue
			}
			switch {
			case kind != "attribute" && kind != "operation" && k.owner == "" && similar(k.short, s.short):
				flags = append(flags, Flag{Symbol: s.name, Similar: k.name, Reason: "near-identical entity name"})
			case kind == "attribute" && k.owner == s.owner && k.writes == "" && similar(k.short, s.short):
				flags = append(flags, Flag{Symbol: s.name, Similar: k.name, Reason: "near-identical attribute name on the same entity"})
			case kind == "attribute" && k.owner == s.owner && s.values != "" && k.values == s.values:
				flags = append(flags, Flag{Symbol: s.name, Similar: k.name, Reason: "same enum values on the same entity"})
			case kind == "operation" && k.owner == s.owner && k.writes != "" && similar(k.short, s.short):
				flags = append(flags, Flag{Symbol: s.name, Similar: k.name, Reason: "near-identical operation name on the same entity"})
			case kind == "operation" && k.owner == s.owner && s.writes != "" && s.writes != "-" && k.writes == s.writes && sameVerb(k.short, s.short):
				// Distinct steps often write the same state (add and update,
				// stop and delete); only the same verb makes a duplicate
				// likely (ISSUES 15).
				flags = append(flags, Flag{Symbol: s.name, Similar: k.name, Reason: "same verb and writes exactly the same attributes"})
			}
		}
		known = append(known, s)
	}
	for _, e := range p.Entities {
		check(symbol{name: e.Name, short: e.Name}, "entity")
		for _, a := range e.Attributes {
			check(attrSymbol(e.Name, a.Name, a.Values), "attribute")
		}
	}
	for _, e := range p.Extensions {
		for _, a := range e.Attributes {
			check(attrSymbol(e.Name, a.Name, a.Values), "attribute")
		}
	}
	for _, o := range p.Operations {
		check(opSymbol(o.Name, o.Writes), "operation")
	}
	sort.Slice(flags, func(i, j int) bool { return flags[i].Symbol < flags[j].Symbol })
	return flags
}

func attrSymbol(entity, name string, values []string) symbol {
	v := append([]string{}, values...)
	sort.Strings(v)
	return symbol{name: entity + "." + name, short: name, owner: entity, values: strings.Join(v, ",")}
}

// verbs groups verbs that name the same kind of step.
var verbs = [][]string{
	{"create", "add", "new", "register", "insert", "make"},
	{"delete", "remove", "destroy", "drop", "purge", "erase"},
	{"update", "edit", "modify", "change", "set", "patch"},
	{"stop", "halt", "terminate", "kill", "shutdown"},
	{"start", "launch", "run", "begin", "spawn", "boot"},
}

// sameVerb reports whether two operation names begin with the same verb or
// with verbs of one group: "stopOnIdle" and "haltIdle", not "add" and "update".
func sameVerb(a, b string) bool {
	va, vb := firstWord(a), firstWord(b)
	if va == vb {
		return true
	}
	for _, g := range verbs {
		if slices.Contains(g, va) && slices.Contains(g, vb) {
			return true
		}
	}
	return false
}

func firstWord(name string) string {
	for i, r := range name {
		if i > 0 && (unicode.IsUpper(r) || r == '_') {
			return strings.ToLower(name[:i])
		}
	}
	return strings.ToLower(name)
}

func opSymbol(name string, writes []string) symbol {
	ent, op, _ := strings.Cut(name, ".")
	w := append([]string{}, writes...)
	sort.Strings(w)
	joined := strings.Join(w, ",")
	if joined == "" {
		joined = "-" // distinguishes operations from attributes in the table
	}
	return symbol{name: name, short: op, owner: ent, writes: joined}
}
