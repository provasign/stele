package vocab

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// Gap is a statement the translator could not fully express: the terms it
// named as missing, and its reasons.
type Gap struct {
	Statement translate.Statement
	Missing   []string
	Rationale []string
}

// Mapping says an existing symbol already means a term the translator
// asked for. The translator gets it as a note on the next attempt.
type Mapping struct {
	StatementID string `json:"statement_id"`
	Term        string `json:"term"`
	Symbol      string `json:"symbol"`
	Reason      string `json:"reason"`
}

// Extend closes translation gaps: for each missing term, the model either
// maps it to an existing symbol or proposes the symbol it needs, following
// the same rules as Propose. acc is the vocabulary proposed so far; the
// result holds only the new symbols. Mappings to symbols that do not exist
// are dropped.
func Extend(ctx context.Context, m llm.Client, gaps []Gap, acc Proposal, opt Options, rep *Report) (Proposal, []Mapping, error) {
	if opt.BatchSize <= 0 {
		opt.BatchSize = 40
	}
	if opt.MaxRepairs == 0 {
		opt.MaxRepairs = 2
	}
	var added Proposal
	var maps []Mapping
	for i := 0; i < len(gaps); i += opt.BatchSize {
		batch := gaps[i:min(i+opt.BatchSize, len(gaps))]
		rep.Batches++
		opt.logf("  gaps %d-%d", i+1, i+len(batch))
		cur := merge(acc, added)
		p, ms, err := extendBatch(ctx, m, batch, cur, opt, rep)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("gaps %s..%s: %v", batch[0].Statement.ID, batch[len(batch)-1].Statement.ID, err))
			continue
		}
		added = merge(added, p)
		maps = append(maps, ms...)
	}
	return added, maps, nil
}

func extendBatch(ctx context.Context, m llm.Client, batch []Gap, acc Proposal, opt Options, rep *Report) (Proposal, []Mapping, error) {
	current, err := loadWith(opt, acc)
	if err != nil {
		return Proposal{}, nil, fmt.Errorf("vocabulary so far does not load: %w", err)
	}
	system := systemPrompt + translate.RenderVocabulary(current)
	var req strings.Builder
	req.WriteString(`The translator could not express parts of the statements below with the vocabulary above, and named the terms it was missing. For each missing term, do one of these:

- If an existing symbol already means it, under whatever name, add a mapping from the term to that symbol (an attribute as Entity.attribute, an operation as Entity.verb) and propose nothing for it. Check the vocabulary carefully first: a second name for an existing fact hides contradictions.
- Otherwise propose the symbol it needs, by the rules above: an attribute on the entity the fact is about, an operation for an action or event (with everything it writes), a relation, or a new enum value through an extension that redeclares the attribute with all its values.
- If the term is not a fact of the system or its world (a wording, a document section, a placeholder), do neither.

`)
	for _, g := range batch {
		fmt.Fprintf(&req, "statement_id: %s\ntext: %s\nmissing: %s\n", g.Statement.ID, llm.Data(g.Statement.Prose), strings.Join(g.Missing, "; "))
		for _, r := range g.Rationale {
			if r != "" {
				fmt.Fprintf(&req, "translator: %s\n", r)
			}
		}
		if rel := translate.Related(current, g.Statement.Prose+" "+strings.Join(g.Missing, " "), 12); len(rel) > 0 {
			fmt.Fprintf(&req, "related symbols: %s\n", strings.Join(rel, ", "))
		}
		req.WriteString("\n")
	}
	turns := []llm.Turn{{Role: "user", Text: req.String()}}
	for round := 0; ; round++ {
		reply, err := m.Complete(ctx, llm.Request{System: system, Turns: turns, Schema: extendSchema})
		if err != nil {
			return Proposal{}, nil, err
		}
		rep.Rounds++
		rep.Usage.Add(reply.Usage)
		var out struct {
			Proposal
			Mappings []Mapping `json:"mappings"`
		}
		if err := json.Unmarshal([]byte(reply.JSON), &out); err != nil {
			return Proposal{}, nil, fmt.Errorf("reply is not the expected JSON: %w", err)
		}
		reg, lerr := loadWith(opt, merge(acc, out.Proposal))
		if lerr == nil {
			return out.Proposal, validMappings(reg, out.Mappings), nil
		}
		if round >= opt.MaxRepairs {
			// Keep what loads: one bad member must not lose the batch's
			// other proposals (a whole 40-statement batch was dropped on
			// held-out NLI-RE Library, leaving 51 statements unexpressed).
			kept, dropped := salvage(opt, acc, out.Proposal)
			if len(kept.Entities)+len(kept.Extensions)+len(kept.Operations) == 0 {
				return Proposal{}, nil, lerr
			}
			rep.Errors = append(rep.Errors, fmt.Sprintf("kept the parts of a proposal that compile, without %s: %v", strings.Join(dropped, ", "), lerr))
			reg, err := loadWith(opt, merge(acc, kept))
			if err != nil {
				return Proposal{}, nil, lerr
			}
			return kept, validMappings(reg, out.Mappings), nil
		}
		turns = append(turns, llm.Turn{Role: "assistant", Text: reply.JSON}, llm.Turn{Role: "user", Text: "The proposal does not compile with the vocabulary:\n" + lerr.Error() +
			"\n\nReturn the complete corrected reply for this batch. Reuse existing symbols instead of redeclaring them; add to an existing entity through extensions."})
	}
}

// validMappings keeps mappings whose symbol is a declared attribute,
// operation or entity.
func validMappings(reg *ir.Registry, ms []Mapping) []Mapping {
	var out []Mapping
	for _, mp := range ms {
		ent, rest, dotted := strings.Cut(mp.Symbol, ".")
		e := reg.Entities[ent]
		switch {
		case e == nil:
		case !dotted:
			out = append(out, mp)
		case e.Attrs[rest] != nil || e.Relations[rest] != nil || reg.Operations[mp.Symbol] != nil:
			out = append(out, mp)
		}
	}
	return out
}

// Merged returns the proposal acc with add appended: the vocabulary after an
// Extend.
func Merged(acc, add Proposal) Proposal { return merge(acc, add) }

// Load compiles a proposal together with the vocabulary files.
func Load(opt Options, p Proposal) (*ir.Registry, error) { return loadWith(opt, p) }

var extendSchema = strings.Replace(outputSchema,
	`"required": ["entities", "extensions", "operations"],
  "properties": {`,
	`"required": ["entities", "extensions", "operations", "mappings"],
  "properties": {
    "mappings": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["statement_id", "term", "symbol", "reason"],
        "properties": {"statement_id": {"type": "string"}, "term": {"type": "string"}, "symbol": {"type": "string"}, "reason": {"type": "string"}}
      }
    },`, 1)

// salvage keeps what of p compiles together with acc: whole entities,
// extensions and operations first, then member by member. It returns the
// kept proposal and the names of what it dropped.
func salvage(opt Options, acc, p Proposal) (Proposal, []string) {
	var kept Proposal
	var dropped []string
	loads := func(k Proposal) bool { _, err := loadWith(opt, merge(acc, k)); return err == nil }
	with := func(k Proposal, x Entity, extension bool) Proposal {
		if extension {
			k.Extensions = append(append([]Entity{}, k.Extensions...), x)
		} else {
			k.Entities = append(append([]Entity{}, k.Entities...), x)
		}
		return k
	}
	for _, extension := range []bool{false, true} {
		es := p.Entities
		if extension {
			es = p.Extensions
		}
		for _, e := range es {
			if next := with(kept, e, extension); loads(next) {
				kept = next
				continue
			}
			x := e
			x.Attributes, x.Relations = nil, nil
			if !loads(with(kept, x, extension)) {
				dropped = append(dropped, e.Name)
				continue
			}
			for _, a := range e.Attributes {
				y := x
				y.Attributes = append(append([]Attribute{}, x.Attributes...), a)
				if loads(with(kept, y, extension)) {
					x = y
				} else {
					dropped = append(dropped, e.Name+"."+a.Name)
				}
			}
			for _, r := range e.Relations {
				y := x
				y.Relations = append(append([]Relation{}, x.Relations...), r)
				if loads(with(kept, y, extension)) {
					x = y
				} else {
					dropped = append(dropped, e.Name+"."+r.Name)
				}
			}
			kept = with(kept, x, extension)
		}
	}
	for _, o := range p.Operations {
		try := func(o Operation) bool {
			next := kept
			next.Operations = append(append([]Operation{}, kept.Operations...), o)
			if loads(next) {
				kept = next
				return true
			}
			return false
		}
		if try(o) {
			continue
		}
		var writes []string
		for _, w := range o.Writes {
			one := o
			one.Writes = append(append([]string{}, writes...), w)
			next := kept
			next.Operations = append(append([]Operation{}, kept.Operations...), one)
			if loads(next) {
				writes = append(writes, w)
			} else {
				dropped = append(dropped, o.Name+" writes "+w)
			}
		}
		o.Writes = writes
		if !try(o) {
			dropped = append(dropped, o.Name)
		}
	}
	return kept, dropped
}
