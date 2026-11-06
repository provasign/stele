package formalize

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

var compactKinds = []struct {
	kind string
	keys []string
}{
	{"invariant", []string{"subject", "on", "when", "assert", "overrides"}},
	{"permission", []string{"operation", "effect", "when", "overrides"}},
	{"precondition", []string{"operation", "when", "overrides"}},
	{"derivation", []string{"assert"}},
	{"informal", []string{"rationale"}},
	{"obligation", []string{"rationale"}},
	{"needs_vocabulary", []string{"rationale", "missing_terms"}},
}

// compactSchema keeps the vocabulary contract, but uses a tagged union for
// rules: the model emits only fields meaningful for that kind. Short request
// IDs replace long source anchors; the compiler restores provenance locally.
func compactSchema() (string, error) {
	combined, err := combinedSchema()
	if err != nil {
		return "", err
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(combined), &schema); err != nil {
		return "", err
	}
	rules := schema["properties"].(map[string]any)["rules"].(map[string]any)
	fields := rules["items"].(map[string]any)["properties"].(map[string]any)
	var variants []any
	for _, v := range compactKinds {
		props := map[string]any{
			"statement_id": fields["statement_id"],
			"kind":         map[string]any{"type": "string", "enum": []string{v.kind}},
		}
		required := []string{"statement_id", "kind"}
		for _, k := range v.keys {
			props[k] = fields[k]
			if k == "overrides" {
				props[k] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
			}
			required = append(required, k)
		}
		if v.kind == "permission" {
			props["effect"] = map[string]any{"type": "string", "enum": []string{"allow", "deny", "possible"}}
		}
		variants = append(variants, map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": props})
	}
	rules["items"] = map[string]any{"anyOf": variants}
	js, err := json.Marshal(schema)
	return string(js), err
}

type compactReply struct {
	vocab.Proposal
	Rules []translate.Candidate `json:"rules"`
}

func decodeCompact(text string, aliases map[string]string) (compactReply, error) {
	var out compactReply
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
