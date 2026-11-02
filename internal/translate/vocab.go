package translate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/ir"
)

// RenderVocabulary prints the symbol table the translator may use, in a
// stable order so the prompt prefix caches.
func RenderVocabulary(reg *ir.Registry) string {
	var b strings.Builder
	names := make([]string, 0, len(reg.Entities))
	for n := range reg.Entities {
		names = append(names, n)
	}
	sort.Strings(names)
	var attrs func(e *ir.Entity, prefix string)
	attrs = func(e *ir.Entity, prefix string) {
		for _, an := range e.AttrNames() {
			a := e.Attrs[an]
			if a.Kind == ir.Enum && len(a.Values) == 0 {
				continue // a platform skeleton the project did not extend
			}
			by := "set by the environment"
			if ws := writers(reg, a); len(ws) > 0 {
				by = "written by " + strings.Join(ws, ", ")
			}
			line := fmt.Sprintf("  %s%s: %s [%s]", prefix, an, a.TypeString(), by)
			if a.Meaning != "" {
				line += " - " + a.Meaning
			}
			b.WriteString(line + "\n")
		}
		// A record's fields are shown under their owner: result.code.
		for _, rn := range sortedKeys(e.Relations) {
			if r := e.Relations[rn]; r.Record {
				attrs(reg.Entities[r.To], prefix+rn+".")
			}
		}
	}
	for _, n := range names {
		e := reg.Entities[n]
		if strings.Contains(n, "$") {
			continue // a record's fields, shown with its owner
		}
		root := lowerFirst(n)
		fmt.Fprintf(&b, "Entity %s (context %s; root `%s`, or `self` when it is the subject)\n", n, e.Context, root)
		attrs(e, "")
		rels := make([]string, 0, len(e.Relations))
		for rn, r := range e.Relations {
			switch {
			case r.Record:
			case r.Many:
				rels = append(rels, fmt.Sprintf("%s -> many %s (checked over up to %d others; use all, any or count)", rn, r.To, r.Max))
			case r.Optional:
				rels = append(rels, rn+" -> "+r.To+" (optional: isSet, none)")
			default:
				rels = append(rels, rn+" -> "+r.To)
			}
		}
		sort.Strings(rels)
		for _, r := range rels {
			fmt.Fprintf(&b, "  relation %s\n", r)
		}
	}
	ops := make([]string, 0, len(reg.Operations))
	for n := range reg.Operations {
		ops = append(ops, n)
	}
	sort.Strings(ops)
	if len(ops) > 0 {
		b.WriteString("\nOperations:\n")
		for _, n := range ops {
			op := reg.Operations[n]
			var ws []string
			for _, a := range op.Writes {
				ws = append(ws, a.Path())
			}
			line := fmt.Sprintf("  %s (default %s; writes %s)", n, op.Default, strings.Join(ws, ", "))
			if op.From != nil {
				line += fmt.Sprintf("; runs only from %s in {%s}", op.From.Attr.QName(), strings.Join(op.From.Values, ", "))
			}
			for _, e := range op.Effect {
				line += fmt.Sprintf("; sets %s = %s", e.Attr.QName(), e.Value)
			}
			b.WriteString(line + "\n")
		}
	}
	if len(reg.Rules) > 0 {
		b.WriteString("\nWorld facts already in the registry (do not translate these again):\n")
		for _, r := range reg.Rules {
			line := fmt.Sprintf("  %s (%s)", r.ID, r.Kind)
			if r.On != "" {
				line += " on " + r.On
			}
			if r.When != nil {
				line += " when " + r.When.String()
			}
			if r.Assert != nil {
				line += " assert " + r.Assert.String()
			}
			if r.Prose != "" {
				line += fmt.Sprintf("  -- %q", r.Prose)
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func writers(reg *ir.Registry, a *ir.Attr) []string {
	var out []string
	for n, op := range reg.Operations {
		if op.WritesAttr(a) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
