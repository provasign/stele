package formalize

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

type compactKind struct {
	kind string
	keys []string
}

var compactKinds = []compactKind{
	{"invariant", []string{"subject", "on", "when", "assert", "overrides"}},
	{"permission", []string{"operation", "effect", "when", "overrides"}},
	{"precondition", []string{"operation", "when", "overrides"}},
	{"derivation", []string{"assert"}},
	{"informal", []string{"rationale"}},
	{"obligation", []string{"rationale"}},
	{"needs_vocabulary", []string{"rationale", "missing_terms"}},
}

// compactSchema keeps the vocabulary contract, but uses a tagged union for
// rules: the model emits only fields meaningful for that kind. Rules come
// grouped under their statement, which names its statement_id and its
// overrides once rather than in every rule: the statement_id, the kind
// and an empty overrides list were a fifth of a reply's rules (measured
// 2026-11-07). Short request IDs replace long source anchors; the
// compiler restores provenance locally.
func compactSchema() (string, error) {
	combined, err := combinedSchema()
	if err != nil {
		return "", err
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(combined), &schema); err != nil {
		return "", err
	}
	props := schema["properties"].(map[string]any)
	fields := props["rules"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	var variants []any
	for _, v := range compactKinds {
		vp := map[string]any{"kind": map[string]any{"type": "string", "enum": []string{v.kind}}}
		required := []string{"kind"}
		for _, k := range v.keys {
			if k == "overrides" {
				continue // the statement's
			}
			vp[k] = fields[k]
			required = append(required, k)
		}
		if v.kind == "permission" {
			vp["effect"] = map[string]any{"type": "string", "enum": []string{"allow", "deny", "possible"}}
		}
		variants = append(variants, map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": vp})
	}
	delete(props, "rules")
	props["statements"] = map[string]any{"type": "array", "items": map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"statement_id", "overrides", "rules"},
		"properties": map[string]any{
			"statement_id": fields["statement_id"],
			"overrides":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"rules":        map[string]any{"type": "array", "items": map[string]any{"anyOf": variants}},
		},
	}}
	var required []any
	for _, r := range schema["required"].([]any) {
		if r != "rules" {
			required = append(required, r)
		}
	}
	schema["required"] = append(required, "statements")
	js, err := json.Marshal(schema)
	return string(js), err
}

// flatCompact turns a reply's statements, each with its rules, into the
// flat rules list (each rule with its statement_id and overrides) the rest
// of the compiler reads. A reply that already has rules is left as it is.
func flatCompact(text string) (string, error) {
	var reply map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &reply); err != nil {
		return "", err
	}
	grouped, ok := reply["statements"]
	if !ok {
		return text, nil
	}
	var statements []struct {
		StatementID string                       `json:"statement_id"`
		Overrides   []string                     `json:"overrides"`
		Rules       []map[string]json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(grouped, &statements); err != nil {
		return "", fmt.Errorf("statements: %w", err)
	}
	rules := []map[string]json.RawMessage{}
	for _, s := range statements {
		id, _ := json.Marshal(s.StatementID)
		for _, r := range s.Rules {
			if _, ok := r["statement_id"]; ok {
				return "", fmt.Errorf("a rule of %s names its own statement_id", s.StatementID)
			}
			if _, ok := r["overrides"]; ok {
				return "", fmt.Errorf("a rule of %s has its own overrides: they are the statement's", s.StatementID)
			}
			r["statement_id"] = id
			var kind string
			_ = json.Unmarshal(r["kind"], &kind)
			if len(s.Overrides) > 0 && slices.ContainsFunc(compactKinds, func(v compactKind) bool {
				return v.kind == kind && slices.Contains(v.keys, "overrides")
			}) {
				r["overrides"], _ = json.Marshal(s.Overrides)
			}
			rules = append(rules, r)
		}
	}
	delete(reply, "statements")
	reply["rules"], _ = json.Marshal(rules)
	js, err := json.Marshal(reply)
	return string(js), err
}

type compactReply struct {
	vocab.Proposal
	Rules []translate.Candidate `json:"rules"`
}

func decodeCompact(text string, aliases map[string]string) (compactReply, error) {
	var out compactReply
	text, err := flatCompact(text)
	if err != nil {
		return out, err
	}
	// Unknown fields must not silently discard a condition or other constraint.
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return out, fmt.Errorf("expected exactly one JSON reply")
	}
	// Enforce the tagged union even for backends without schema enforcement.
	// In particular an informal entry must not hide a supplied constraint.
	var raw struct {
		Rules []map[string]json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return out, err
	}
	if raw.Rules == nil {
		return out, fmt.Errorf("reply has no rules array")
	}
	for i, c := range out.Rules {
		allowed := map[string]bool{"statement_id": true, "kind": true}
		for _, v := range compactKinds {
			if v.kind == c.Kind {
				for _, k := range v.keys {
					allowed[k] = true
				}
			}
		}
		for k := range raw.Rules[i] {
			if !allowed[k] {
				return out, fmt.Errorf("field %q is not allowed for %s", k, c.Kind)
			}
		}
		for k := range allowed {
			if k == "overrides" {
				continue // optional: absent means the rule overrides nothing
			}
			if value, ok := raw.Rules[i][k]; !ok || string(value) == "null" {
				return out, fmt.Errorf("missing %s field for %s", k, c.Kind)
			}
		}
		if c.Kind == "permission" && c.Effect != "allow" && c.Effect != "deny" && c.Effect != "possible" {
			return out, fmt.Errorf("invalid permission effect %q", c.Effect)
		}
	}
	resolve := func(id string) (string, error) {
		if original, ok := aliases[id]; ok {
			return original, nil
		}
		return "", fmt.Errorf("unknown statement_id %q", id)
	}
	for i := range out.Rules {
		c := &out.Rules[i]
		var err error
		if c.StatementID, err = resolve(c.StatementID); err != nil {
			return out, err
		}
		for k, id := range c.Overrides {
			if original, ok := aliases[id]; ok {
				c.Overrides[k] = original
			} // else a label the text uses ("FR-005"), resolved in its document by resolveOverrides
		}
		switch c.Kind {
		case "invariant", "permission", "precondition", "derivation":
			c.Outcome = "rule"
		case "informal", "needs_vocabulary", "obligation":
			c.Outcome, c.Kind = c.Kind, "none"
		default:
			return out, fmt.Errorf("unknown rule kind %q", c.Kind)
		}
	}
	for _, entities := range [][]vocab.Entity{out.Entities, out.Extensions} {
		for i := range entities {
			for k, id := range entities[i].StatementIDs {
				original, err := resolve(id)
				if err != nil {
					return out, err
				}
				entities[i].StatementIDs[k] = original
			}
		}
	}
	for i := range out.Operations {
		for k, id := range out.Operations[i].StatementIDs {
			original, err := resolve(id)
			if err != nil {
				return out, err
			}
			out.Operations[i].StatementIDs[k] = original
		}
	}
	return out, nil
}
