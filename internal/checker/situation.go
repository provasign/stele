package checker

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/provasign/stele/internal/ir"
)

// fresh matches the strings a solver invents for "some text other than the
// rules' literals", like "!14!".
var fresh = regexp.MustCompile(`^"!(\d+)!"$`)

// valuePart splits a variable's name from a trailing part: presence ("?"),
// a length ("#n") or a position ("#2").
var valuePart = regexp.MustCompile(`^(.+?)(\?|#n|#\d+)$`)

// Situation renders a counterexample as values a spec author reads: a list
// or set from its length and positions, an optional that is unset as unset,
// and a string the solver invented as other text.
func Situation(reg *ir.Registry, bs []Binding) []string {
	attrs := map[string]*ir.Attr{}
	if reg == nil {
		reg = &ir.Registry{}
	}
	for _, e := range reg.Entities {
		for _, a := range e.Attrs {
			attrs[a.QName()] = a
		}
	}
	type value struct {
		set, unset bool
		plain      string
		n          int
		elems      map[int]string
	}
	vals := map[string]*value{}
	var order []string
	get := func(name string) *value {
		if vals[name] == nil {
			vals[name] = &value{n: -1, elems: map[int]string{}}
			order = append(order, name)
		}
		return vals[name]
	}
	text := func(s string) string {
		if m := fresh.FindStringSubmatch(s); m != nil {
			return "<other text " + m[1] + ">"
		}
		return s
	}
	for _, b := range bs {
		// Only a trailing part is one: Shed.tools#1.id names an attribute
		// of a slot, and cutting it at "#" merged every slot's values into
		// one, leaving only presence flags to describe the situation.
		name, part := b.Name, ""
		if m := valuePart.FindStringSubmatch(name); m != nil {
			name, part = m[1], m[2]
		}
		v := get(name)
		switch {
		case part == "?":
			v.set, v.unset = true, b.Value == "false"
		case part == "#n":
			v.n, _ = strconv.Atoi(b.Value)
		case strings.HasPrefix(part, "#"):
			if i, err := strconv.Atoi(part[1:]); err == nil {
				v.elems[i] = text(b.Value)
			} else {
				v.plain = text(b.Value)
			}
		default:
			v.plain = text(b.Value)
		}
	}
	var out []string
	for _, name := range order {
		v := vals[name]
		switch {
		case v.set && v.unset:
			out = append(out, name+" unset")
		case v.n >= 0:
			open, end := "[", "]"
			if a := attrs[name]; a != nil && a.Kind == ir.SetKind {
				open, end = "{", "}"
			}
			var es []string
			for i := 0; i < v.n; i++ {
				e, ok := v.elems[i]
				if !ok {
					e = "<any>"
				}
				if open == "{" && slices.Contains(es, e) {
					continue // a set's positions may repeat a member
				}
				es = append(es, e)
			}
			out = append(out, fmt.Sprintf("%s=%s%s%s", name, open, strings.Join(es, ", "), end))
		case v.plain != "":
			out = append(out, name+"="+v.plain)
		}
	}
	return out
}

// Plain renders a situation in words: "Cluster.flushRecorded=false" as
// "not cluster: flush recorded", whether a step runs
// ("Workspace.delete:runs=true") as "workspace: delete runs", first, and the
// values after the step ("Workspace.status'=running") last, as "after the
// step, workspace: status is running" (ISSUES 10).
func Plain(s []string) string {
	var now, runs, after []string
	for _, b := range s {
		name, val, ok := strings.Cut(b, "=")
		if !ok {
			name, val = strings.TrimSuffix(b, " unset"), "unset"
		}
		if slot, ok := strings.CutSuffix(name, ":in"); ok {
			switch val {
			case "true":
				now = append(now, Words(slot)+" exists")
			case "false":
				now = append(now, "no "+Words(slot))
			}
			continue
		}
		if op, ok := strings.CutSuffix(name, ":runs"); ok {
			switch val {
			case "true":
				runs = append(runs, Words(op)+" runs")
			case "false":
				runs = append(runs, Words(op)+" does not run")
			}
			continue
		}
		into := &now
		if n, ok := strings.CutSuffix(name, "'"); ok {
			name, into = n, &after
		}
		subject := Words(name)
		switch val {
		case "true":
			*into = append(*into, subject)
		case "false":
			*into = append(*into, "not "+subject)
		case `""`:
			*into = append(*into, subject+" is empty")
		default:
			*into = append(*into, subject+" is "+SplitName(val))
		}
	}
	out := strings.Join(append(runs, now...), ", ")
	if len(after) > 0 {
		if out != "" {
			out += "; "
		}
		out += "after the step, " + strings.Join(after, ", ")
	}
	return out
}

// Words is a symbol in words: "Workspace.idleSince" as "workspace: idle since".
func Words(symbol string) string {
	ent, name, ok := strings.Cut(symbol, ".")
	if !ok {
		return SplitName(symbol)
	}
	// A slot of a relation to many, "Shed.tools#2.id": "shed: tools #2: id";
	// of instances(E), "Tool.*instances#1.id": "tool: other #1: id".
	var parts []string
	for _, seg := range strings.Split(name, ".") {
		if rel, n, ok := strings.Cut(seg, "#"); ok {
			if rel == "*instances" {
				rel = "other"
			}
			parts = append(parts, SplitName(rel)+" #"+n)
			continue
		}
		parts = append(parts, SplitName(seg))
	}
	return SplitName(ent) + ": " + strings.Join(parts, ": ")
}

// SplitName splits a camel-case or snake-case name into lower-case words.
func SplitName(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(rs[i-1]) || i+1 < len(rs) && unicode.IsLower(rs[i+1])) {
			b.WriteRune(' ')
		}
		if r == '_' {
			r = ' '
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
