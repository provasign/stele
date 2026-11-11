package vocab

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// FactMap asks once, over every attribute the rules use, which attributes
// state one fact under different names, on any entities, and which facts
// constrain each other by what the things are (ISSUES 16, 17). Pairs chosen
// by name similarity miss synonyms whose names share nothing (a payment
// taken, an invoice paid, a charge captured); a model
// that sees the whole list does not need the pair offered. setBy says who
// sets each attribute, by name ("written by Order.pay", "the world").
func FactMap(ctx context.Context, m llm.Client, base *ir.Registry, used []Used, setBy map[string]string, rep *Report) ([]Merge, []Link, error) {
	if len(used) < 2 {
		return nil, nil, nil
	}
	byEntity := map[string][]Used{}
	var ents []string
	for _, u := range used {
		e, _, _ := strings.Cut(u.Name, ".")
		if byEntity[e] == nil {
			ents = append(ents, e)
		}
		byEntity[e] = append(byEntity[e], u)
	}
	sort.Strings(ents)
	var b strings.Builder
	b.WriteString(factMapRequest)
	for _, e := range ents {
		fmt.Fprintf(&b, "Entity %s\n", e)
		for _, u := range byEntity[e] {
			fmt.Fprintf(&b, "  %s: %s", u.Name, u.Type)
			if s := setBy[u.Name]; s != "" {
				fmt.Fprintf(&b, " [%s]", s)
			}
			if u.Meaning != "" {
				fmt.Fprintf(&b, " - %s", llm.Data(u.Meaning))
			}
			if len(u.Examples) > 0 {
				fmt.Fprintf(&b, "\n      used by: %s", u.Examples[0])
			}
			b.WriteString("\n")
		}
	}
	reply, err := m.Complete(ctx, llm.Request{
		System: consolidatePrompt + translate.RenderVocabulary(base),
		Turns:  []llm.Turn{{Role: "user", Text: b.String()}},
		Schema: factMapSchema,
	})
	if err != nil {
		return nil, nil, err
	}
	rep.Rounds++
	rep.Usage.Add(reply.Usage)
	var out struct {
		Groups []struct {
			Keep       string   `json:"keep"`
			Attributes []string `json:"attributes"`
			Reason     string   `json:"reason"`
		} `json:"same_fact"`
		Links []Link `json:"links"`
	}
	if err := json.Unmarshal([]byte(reply.JSON), &out); err != nil {
		return nil, nil, fmt.Errorf("fact map: reply is not the expected JSON: %v", err)
	}
	known := map[string]bool{}
	for _, u := range used {
		known[u.Name] = true
	}
	var merges []Merge
	for _, g := range out.Groups {
		if !known[g.Keep] {
			continue
		}
		for _, a := range g.Attributes {
			if a != g.Keep && known[a] {
				merges = append(merges, Merge{From: a, Into: g.Keep, Reason: g.Reason})
			}
		}
	}
	var links []Link
	for _, l := range out.Links {
		if l.Assert != "" {
			l.A, l.B = "fact map", ""
			links = append(links, l)
		}
	}
	return merges, links, nil
}

const factMapRequest = `Below is every attribute the translated requirements use, by entity, with who sets it and a requirement that uses it. Requirements written by different people name one fact differently, often on different entities (the order's payment taken, the invoice's paid flag). Rules only meet, and contradictions only show, where they share a fact or a fact ties them.

Return:
- same_fact: groups of attributes that state the same fact about the same thing in the same terms (same type and, for enums, the same values), whatever their names or entities. Name the one to keep: prefer the one a step writes, then the most used. Two attributes that are opposites, or differ in kind, are not one fact.
- links: facts that constrain each other by what the things are, not by what a requirement wants: one lives inside or is part of the other (an order's items are in its warehouse), one exists only while the other does, one is defined by the other. Each is an invariant in "assert" (and "when", or empty), with paths from each entity's root in lower camel case (warehouse.status, order.itemsAvailable), with =, !=, in {..}, not, and, or, implies. Use "pair" 0.

Most attributes are neither. A wrong merge or link invents contradictions; a missing one hides them.

`

const factMapSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["same_fact", "links"],
  "properties": {
    "same_fact": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["keep", "attributes", "reason"],
        "properties": {"keep": {"type": "string"}, "attributes": {"type": "array", "items": {"type": "string"}}, "reason": {"type": "string"}}
      }
    },
    "links": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["pair", "when", "assert", "reason"],
        "properties": {"pair": {"type": "integer"}, "when": {"type": "string"}, "assert": {"type": "string"}, "reason": {"type": "string"}}
      }
    }
  }
}`
