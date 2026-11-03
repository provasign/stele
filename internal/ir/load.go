package ir

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// FormatVersion is the IR format version this loader reads.
const FormatVersion = 1

type rawAttr struct {
	Type         string      `yaml:"type"`
	Values       []string    `yaml:"values"`
	Range        []yaml.Node `yaml:"range"`
	ControlledBy string      `yaml:"controlledBy"`
	Meaning      string      `yaml:"meaning"`
	SetBy        string      `yaml:"setBy"` // "world": no operation may write it
	// Of is a list's or set's element type, or a map's value type; Values
	// and Range then apply to it. Key and KeyValues are a map's key type.
	Of        string   `yaml:"of"`
	Key       string   `yaml:"key"`
	KeyValues []string `yaml:"keyValues"`
	MaxLength int      `yaml:"maxLength"`
	Optional  bool     `yaml:"optional"`
	// Fields are a record's attributes.
	Fields map[string]rawAttr `yaml:"fields"`
}

type rawRelation struct {
	To       string `yaml:"to"`
	Inverse  string `yaml:"inverse"`
	Many     bool   `yaml:"many"`
	Max      int    `yaml:"max"`
	Optional bool   `yaml:"optional"`
}

type rawStateMachine struct {
	Field       string `yaml:"field"`
	Transitions []struct {
		From yaml.Node `yaml:"from"` // a state or a list of states
		To   string    `yaml:"to"`
		Via  string    `yaml:"via"`
	} `yaml:"transitions"`
}

type rawSupersede struct {
	Winner string `yaml:"winner"`
	Loser  string `yaml:"loser"`
	When   string `yaml:"when"`
}

// rawDoc is the union of every document kind; which keys are set says which
// kind it is.
type rawDoc struct {
	Context       string                 `yaml:"context"`
	FormatVersion int                    `yaml:"formatVersion"`
	Owner         string                 `yaml:"owner"`
	Entity        string                 `yaml:"entity"`
	Extend        string                 `yaml:"extend"`
	Operation     string                 `yaml:"operation"`
	Attributes    map[string]rawAttr     `yaml:"attributes"`
	Relations     map[string]rawRelation `yaml:"relations"`
	StateMachine  *rawStateMachine       `yaml:"stateMachine"`
	CreatedBy     []string               `yaml:"createdBy"` // on an entity or extend: the operations that create it

	Default string   `yaml:"default"`
	Writes  []string `yaml:"writes"`

	ID      string `yaml:"id"`
	On      string `yaml:"on"`
	Assume  bool   `yaml:"assume"`
	Version int    `yaml:"version"`
	Kind    string `yaml:"kind"`
	// Overrides names statements a rule takes precedence over, as its text says.
	Overrides []string  `yaml:"overrides"`
	Effect    yaml.Node `yaml:"effect"` // allow|deny on a permission; a map of assignments on an operation
	Subject   string    `yaml:"subject"`
	When      string    `yaml:"when"`
	Assert    string    `yaml:"assert"`
	Status    string    `yaml:"status"`
	Prose     string    `yaml:"prose"`
	Source    struct {
		StatementID     string `yaml:"statementId"`
		StatementHash   string `yaml:"statementHash"`
		Doc             string `yaml:"doc"`
		Prose           string `yaml:"prose"`
		TextHash        string `yaml:"textHash"`
		Introduced      string `yaml:"introduced"`
		ApprovedBy      string `yaml:"approvedBy"`
		ApprovedHash    string `yaml:"approvedHash"`
		ApprovedReading string `yaml:"approvedReading"`
	} `yaml:"source"`

	Resolves  []string      `yaml:"resolves"`
	Supersede *rawSupersede `yaml:"supersede"`
	Retire    []string      `yaml:"retire"`
	Rationale string        `yaml:"rationale"`

	// A lesson (D-063).
	Lesson     string            `yaml:"lesson"`
	Says       string            `yaml:"says"`
	Statements map[string]string `yaml:"statements"`
	Terms      []string          `yaml:"terms"`
	From       string            `yaml:"from"`
	By         string            `yaml:"by"`
	At         string            `yaml:"at"`

	// Unchecked is a step the owner accepted unchecked (UncheckedStep).
	Unchecked string `yaml:"unchecked"`

	Rules          []string `yaml:"rules"` // an open conflict's pinned set
	Response       string   `yaml:"response"`
	Situation      string   `yaml:"situation"`
	AcknowledgedBy []string `yaml:"acknowledgedBy"`
	Since          string   `yaml:"since"`
	ClosedBy       string   `yaml:"closedBy"`
}

// LoadDir reads every *.yaml and *.yml file under dir (multi-document files
// allowed) and returns the resolved registry. All problems found are joined
// into one error.
func LoadDir(dir string) (*Registry, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ext := filepath.Ext(path); !d.IsDir() && (ext == ".yaml" || ext == ".yml") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .yaml files under %s", dir)
	}
	sort.Strings(files)
	return LoadFiles(files...)
}

// LoadFiles reads the given YAML files as one registry.
func LoadFiles(files ...string) (*Registry, error) {
	l := &loader{reg: newRegistry()}
	for _, f := range files {
		l.file(f)
	}
	l.finish()
	if len(l.errs) > 0 {
		return nil, errors.Join(l.errs...)
	}
	return l.reg, nil
}

func newRegistry() *Registry {
	r := &Registry{
		Contexts:   map[string]*Context{},
		Entities:   map[string]*Entity{},
		Operations: map[string]*Operation{},
	}
	// The built-in platform context ships skeletons only (docs/04: built-in
	// actor and request entities); projects extend them.
	actor := &Entity{Name: "Actor", Context: "platform", Attrs: map[string]*Attr{}, Relations: map[string]*Relation{}}
	actor.Attrs["kind"] = &Attr{Entity: actor, Name: "kind", Kind: Enum}
	actor.Attrs["role"] = &Attr{Entity: actor, Name: "role", Kind: Enum}
	request := &Entity{Name: "Request", Context: "platform", Attrs: map[string]*Attr{}, Relations: map[string]*Relation{}}
	// Result is what a step produces: its outputs, messages, events and
	// return values, read as result.x in the assert of a rule on the step.
	result := &Entity{Name: "Result", Context: "platform", Attrs: map[string]*Attr{}, Relations: map[string]*Relation{}}
	r.Entities["Actor"], r.Entities["Request"], r.Entities["Result"] = actor, request, result
	return r
}

type loader struct {
	reg      *Registry
	errs     []error
	inverses []inverse
	opDocs   []opDoc
	machines []machineDoc
	extends  []extendDoc
	creators []creatorDoc
	// opExtends add writes to operations declared elsewhere.
	opExtends []opDoc
}

type creatorDoc struct {
	entity string
	ops    []string
	where  string
}

type extendDoc struct {
	d     *rawDoc
	where string
}

type machineDoc struct {
	entity string
	sm     *rawStateMachine
	where  string
}

type opDoc struct {
	name   string
	def    string
	writes []string
	effect map[string]any
	where  string
}

type inverse struct {
	from, name, to string
	rel            string // the declared relation it inverts
	where          string
}

func (l *loader) errf(where, format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf("%s: %s", where, fmt.Sprintf(format, args...)))
}

func (l *loader) file(path string) {
	f, err := os.Open(path)
	if err != nil {
		l.errs = append(l.errs, err)
		return
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	for n := 1; ; n++ {
		var node yaml.Node
		if err := dec.Decode(&node); err == io.EOF {
			return
		} else if err != nil {
			l.errf(path, "%v", err)
			return
		}
		where := fmt.Sprintf("%s:%d", path, node.Line)
		if len(node.Content) == 0 || node.Content[0].Tag == "!!null" { // only comments
			continue
		}
		var d rawDoc
		if err := node.Decode(&d); err != nil {
			l.errf(where, "%v", err)
			continue
		}
		l.doc(&d, where)
	}
}

func (l *loader) doc(d *rawDoc, where string) {
	switch {
	case d.Lesson != "":
		l.lesson(d, where)
	case d.Unchecked != "":
		if l.reg.Unchecked == nil {
			l.reg.Unchecked = map[string]*UncheckedStep{}
		}
		l.reg.Unchecked[d.Unchecked] = &UncheckedStep{Step: d.Unchecked, By: d.By, At: d.At, File: where}
	case d.ID != "" && (d.Supersede != nil || len(d.Retire) > 0):
		l.decision(d, where)
	case d.ID != "" && (len(d.Rules) > 0 || d.Response != ""):
		l.openConflict(d, where)
	case d.ID != "":
		l.rule(d, where)
	case d.Entity != "" || d.Extend != "":
		if len(d.CreatedBy) > 0 {
			l.creators = append(l.creators, creatorDoc{entity: strings.TrimPrefix(d.Entity+d.Extend, "platform."), ops: d.CreatedBy, where: where})
		}
		if d.Entity != "" {
			l.entity(d.Entity, d, where, false)
			break
		}
		if strings.Contains(strings.TrimPrefix(d.Extend, "platform."), ".") {
			// extend: Entity.op adds writes to an operation declared elsewhere.
			if len(d.Writes) == 0 {
				l.errf(where, "extend %s: an operation extension lists writes", d.Extend)
				break
			}
			l.opExtends = append(l.opExtends, opDoc{name: strings.TrimPrefix(d.Extend, "platform."), writes: d.Writes, where: where})
			break
		}
		// Applied after every file is read: the entity may be declared later.
		l.extends = append(l.extends, extendDoc{d: d, where: where})
	case d.Operation != "":
		if _, _, ok := strings.Cut(d.Operation, "."); !ok {
			l.errf(where, "operation %q must be Entity.name", d.Operation)
			return
		}
		if d.Default != "allow" && d.Default != "deny" {
			l.errf(where, "operation %s: default must be allow or deny (what happens when no permission rule matches)", d.Operation)
		}
		var effect map[string]any
		if d.Effect.Kind != 0 {
			if err := d.Effect.Decode(&effect); err != nil {
				l.errf(where, "operation %s: effect must map attributes to values: %v", d.Operation, err)
			}
		}
		l.opDocs = append(l.opDocs, opDoc{name: d.Operation, def: d.Default, writes: d.Writes, effect: effect, where: where})
	case d.Context != "":
		if d.FormatVersion != FormatVersion {
			l.errf(where, "context %s: formatVersion %d, this stele reads %d", d.Context, d.FormatVersion, FormatVersion)
		}
		l.reg.Contexts[d.Context] = &Context{Name: d.Context, Owner: d.Owner}
	default:
		l.errf(where, "document is not a context, entity, extend, operation, rule, decision, open conflict or lesson")
	}
}

func (l *loader) lesson(d *rawDoc, where string) {
	switch d.Kind {
	case LessonTerm, LessonFact, LessonReading, LessonScope:
	default:
		l.errf(where, "lesson %s: kind must be term, fact, reading or scope, not %q", d.Lesson, d.Kind)
		return
	}
	if strings.TrimSpace(d.Says) == "" {
		l.errf(where, "lesson %s: says is empty", d.Lesson)
		return
	}
	if d.Kind == LessonReading && len(d.Statements) == 0 {
		l.errf(where, "lesson %s: a reading lesson names the statement it is about (statements)", d.Lesson)
		return
	}
	for _, x := range l.reg.Lessons {
		if x.ID == d.Lesson {
			l.errf(where, "lesson %s is declared twice (also in %s)", d.Lesson, x.File)
			return
		}
	}
	l.reg.Lessons = append(l.reg.Lessons, &Lesson{ID: d.Lesson, Kind: d.Kind, Says: strings.TrimSpace(d.Says), Statements: d.Statements,
		Terms: d.Terms, From: d.From, By: d.By, At: d.At, File: where})
}

func (l *loader) entity(name string, d *rawDoc, where string, extend bool) {
	e := l.reg.Entities[name]
	switch {
	case e == nil:
		e = &Entity{Name: name, Context: d.Context, Attrs: map[string]*Attr{}, Relations: map[string]*Relation{}}
		l.reg.Entities[name] = e
	case !extend:
		l.errf(where, "entity %s declared twice", name)
		return
	}
	// A dotted name declares a record field: result.code is field code of
	// record result.
	attrs := map[string]rawAttr{}
	for an, ra := range d.Attributes {
		head, rest, dotted := strings.Cut(an, ".")
		if !dotted {
			if prev, ok := attrs[an]; ok && len(prev.Fields) > 0 {
				ra.Fields = prev.Fields
			}
			attrs[an] = ra
			continue
		}
		rec := attrs[head]
		if rec.Fields == nil {
			rec.Fields = map[string]rawAttr{}
		}
		if rec.Type == "" {
			rec.Type = "record"
		}
		rec.Fields[rest] = ra
		attrs[head] = rec
	}
	d = &rawDoc{Context: d.Context, Attributes: attrs, Relations: d.Relations, StateMachine: d.StateMachine}
	names := make([]string, 0, len(d.Attributes))
	for an := range d.Attributes {
		names = append(names, an)
	}
	sort.Strings(names)
	for _, an := range names {
		ra := d.Attributes[an]
		where := fmt.Sprintf("%s: %s.%s", where, name, an)
		if strings.EqualFold(baseType(ra.Type), "record") || len(ra.Fields) > 0 {
			l.record(e, an, ra, where)
			continue
		}
		a := e.Attrs[an]
		if a == nil {
			if ra.Type == "" {
				l.errf(where, "missing type")
				continue
			}
			a = &Attr{Entity: e, Name: an}
			e.Attrs[an] = a
		}
		if ra.Type != "" || ra.Of != "" || len(ra.Values) > 0 || len(ra.Range) > 0 || ra.MaxLength > 0 || ra.Key != "" {
			if err := l.setType(a, ra); err != nil {
				l.errf(where, "%v", err)
				continue
			}
		}
		if ra.Optional {
			a.Optional = true
		}
		if ra.Meaning != "" {
			a.Meaning = ra.Meaning
		}
		switch ra.SetBy {
		case "":
		case "world":
			a.WorldSet = true
		default:
			l.errf(where, "setBy must be world (attributes some operation writes are the system's)")
		}
		if ra.ControlledBy != "" {
			l.errf(where, "controlledBy was replaced by operation writes (D-042): list the attribute in `writes` of the operations that set it")
		}
	}
	if d.StateMachine != nil {
		l.machines = append(l.machines, machineDoc{entity: name, sm: d.StateMachine, where: where})
	}
	for rn, rr := range d.Relations {
		rel := &Relation{Name: rn, To: rr.To, Many: rr.Many, Max: rr.Max, Inverse: rr.Inverse, Optional: rr.Optional}
		if rel.Optional {
			if rel.Many {
				l.errf(where, "relation %s.%s: a relation to many may be empty already; optional is for single relations", name, rn)
			}
			rel.Set = &Attr{Entity: e, Name: rn + "?", Kind: Bool}
		}
		if rel.Many && rel.Max <= 0 {
			rel.Max = DefaultMaxInstances
		}
		if !rel.Many && rr.Max != 0 {
			l.errf(where, "relation %s.%s: max is for relations with many: true", name, rn)
		}
		e.Relations[rn] = rel
		if rr.Inverse != "" {
			l.inverses = append(l.inverses, inverse{from: name, name: rr.Inverse, to: rr.To, rel: rn, where: where})
		}
	}
}

// record declares a record attribute's fields as an entity of their own,
// owned by e through a relation of the attribute's name: self.response.status.
func (l *loader) record(e *Entity, name string, ra rawAttr, where string) {
	if len(ra.Fields) == 0 {
		l.errf(where, "a record needs fields")
		return
	}
	if _, clash := e.Attrs[name]; clash {
		l.errf(where, "declared as both an attribute and a record")
		return
	}
	sub := e.Name + "$" + name
	rel := e.Relations[name]
	if rel != nil && (!rel.Record || rel.To != sub) {
		l.errf(where, "declared as both a relation and a record")
		return
	}
	e.Relations[name] = &Relation{Name: name, To: sub, Record: true}
	l.entity(sub, &rawDoc{Context: e.Context, Attributes: ra.Fields}, where, rel != nil)
}

// baseType strips a generic argument: list<string> is list.
func baseType(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.IndexAny(t, "<["); i >= 0 {
		return t[:i]
	}
	return t
}

// typeNames maps the spellings the format and translators use to kinds.
var typeNames = map[string]Kind{
	"bool": Bool, "boolean": Bool,
	"int": Int, "integer": Int,
	"enum":   Enum,
	"string": String, "text": String, "bytes": String,
	"decimal": Decimal, "number": Decimal, "float": Decimal, "double": Decimal, "real": Decimal,
	"duration": Duration,
	"time":     Time, "timestamp": Time, "datetime": Time, "date": Time,
	"list": List, "array": List, "sequence": List,
	"set": SetKind,
	"map": Map, "dict": Map,
}

// setType sets a's type from its declaration. Generic spellings are
// accepted: list<string>, set<enum>, map<string, int>, optional<int>.
func (l *loader) setType(a *Attr, ra rawAttr) error {
	t := strings.TrimSpace(ra.Type)
	if inner, ok := generic(t, "optional"); ok {
		a.Optional, t = true, inner
	}
	args := ""
	if i := strings.IndexAny(t, "<["); i >= 0 && strings.HasSuffix(t, map[byte]string{'<': ">", '[': "]"}[t[i]]) {
		t, args = t[:i], t[i+1:len(t)-1]
	}
	t = strings.ToLower(strings.TrimSpace(t)) // values in args keep their case
	if t == "" {
		t = KindNames[a.Kind] // an extension adding values or a range only
	}
	k, ok := typeNames[t]
	if !ok {
		return fmt.Errorf("type %q is not known (bool, int, enum, string, decimal, duration, time, list, set, map, record; optional: true for a value that may be absent)", ra.Type)
	}
	a.Kind = k
	switch k {
	case Enum:
		if args != "" { // enum[a, b], the legacy spelling
			a.Values = nil
			for _, v := range strings.Split(args, ",") {
				if v = strings.TrimSpace(v); v != "" {
					a.Values = append(a.Values, v)
				}
			}
		}
		if len(ra.Values) > 0 {
			a.Values = ra.Values
		}
	case List, SetKind, Map:
		of, key := ra.Of, ra.Key
		values, keyValues := ra.Values, ra.KeyValues
		if args != "" {
			parts := splitTop(args)
			if k == Map && len(parts) == 2 {
				key, of = parts[0], parts[1]
			} else {
				of = parts[0]
			}
			// Inline enum values: list<enum[a, b]>, map<enum[gold, silver], int>.
			if v, ok := inlineValues(of); ok {
				of, values = "enum", v
			}
			if v, ok := inlineValues(key); ok {
				key, keyValues = "enum", v
			}
		}
		if of == "" {
			return fmt.Errorf("%s needs an element type: `of: string` (or %s<string>)", t, t)
		}
		elem := &Attr{Entity: a.Entity, Name: a.Name + "[]"}
		if err := l.setType(elem, rawAttr{Type: of, Values: values, Range: ra.Range}); err != nil {
			return fmt.Errorf("element: %w", err)
		}
		if elem.Kind == List || elem.Kind == SetKind || elem.Kind == Map {
			return fmt.Errorf("collections of collections are not supported; use a relation to many entities")
		}
		a.Elem = elem
		if k == Map {
			if key == "" {
				return fmt.Errorf("map needs a key type: `key: string` (or map<string, int>)")
			}
			kt := &Attr{Entity: a.Entity, Name: a.Name + "[key]"}
			if err := l.setType(kt, rawAttr{Type: key, Values: keyValues}); err != nil {
				return fmt.Errorf("key: %w", err)
			}
			a.Key = kt
		}
		if k == List || a.Bounded() {
			a.MaxLen = ra.MaxLength
			a.maxLenDeclared = ra.MaxLength > 0
			if a.MaxLen <= 0 {
				a.MaxLen = DefaultMaxLen
			}
		}
		return nil
	}
	if len(ra.Values) > 0 && k != Enum {
		return fmt.Errorf("`values` on a %s attribute (values are for enums)", t)
	}
	if len(ra.Range) > 0 {
		if len(ra.Range) != 2 {
			return fmt.Errorf("range must be [min, max]")
		}
		lo, hi := ra.Range[0].Value, ra.Range[1].Value
		switch k {
		case Int:
			min, err1 := strconv.Atoi(lo)
			max, err2 := strconv.Atoi(hi)
			if err1 != nil || err2 != nil || min > max {
				return fmt.Errorf("range must be [min, max] integers with min <= max")
			}
			a.HasRange, a.Min, a.Max = true, min, max
		case Decimal:
			x, err1 := strconv.ParseFloat(lo, 64)
			y, err2 := strconv.ParseFloat(hi, 64)
			if err1 != nil || err2 != nil || x > y {
				return fmt.Errorf("range must be [min, max] numbers with min <= max")
			}
			a.HasRange, a.RMin, a.RMax = true, lo, hi
		default:
			return fmt.Errorf("range is for int and decimal attributes")
		}
	}
	return nil
}

// splitTop splits generic arguments at commas outside brackets.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '<', '[', '{':
			depth++
		case '>', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

// inlineValues reads enum[a, b] or enum{a, b}.
func inlineValues(t string) ([]string, bool) {
	for _, open := range []string{"enum[", "enum{", "enum {", "enum ["} {
		if strings.HasPrefix(strings.ToLower(t), open) && (strings.HasSuffix(t, "]") || strings.HasSuffix(t, "}")) {
			var out []string
			for _, v := range strings.Split(t[len(open):len(t)-1], ",") {
				if v = strings.TrimSpace(v); v != "" {
					out = append(out, v)
				}
			}
			return out, true
		}
	}
	return nil, false
}

// generic returns the argument of name<arg>.
func generic(t, name string) (string, bool) {
	if strings.HasPrefix(strings.ToLower(t), name+"<") && strings.HasSuffix(t, ">") {
		return strings.TrimSpace(t[len(name)+1 : len(t)-1]), true
	}
	return "", false
}

func (l *loader) rule(d *rawDoc, where string) {
	x := &Rule{
		ID: d.ID, Version: d.Version, Kind: d.Kind, Effect: d.Effect.Value, Operation: d.Operation, On: d.On, Assume: d.Assume,
		Subject: d.Subject, Status: d.Status, Source: d.Source.Doc, Prose: d.Source.Prose,
		StatementID: d.Source.StatementID, StatementHash: d.Source.StatementHash, TextHash: d.Source.TextHash, Introduced: d.Source.Introduced, File: where,
		Overrides: d.Overrides, ApprovedBy: d.Source.ApprovedBy, ApprovedHash: d.Source.ApprovedHash, ApprovedReading: d.Source.ApprovedReading,
		Rationale: strings.TrimSpace(d.Rationale),
	}
	if x.Version == 0 {
		x.Version = 1
	}
	if x.Status == "" {
		x.Status = "active"
	}
	if x.Prose == "" {
		x.Prose = d.Prose
	}
	if x.Source == "" {
		x.Source = where
	}
	if l.reg.Rule(x.ID) != nil {
		l.errf(where, "rule %s declared twice", x.ID)
		return
	}
	switch x.Kind {
	case Permission:
		if x.Effect != "allow" && x.Effect != "deny" && x.Effect != "possible" {
			l.errf(where, "rule %s: permission effect must be allow, deny or possible", x.ID)
		}
		fallthrough
	case Precondition:
		if x.Operation == "" {
			l.errf(where, "rule %s: %s needs an operation", x.ID, x.Kind)
		}
	case Invariant, Derivation:
		if d.Assert == "" {
			l.errf(where, "rule %s: %s needs assert", x.ID, x.Kind)
		}
		if x.Operation != "" {
			l.errf(where, "rule %s: an invariant about a step names it with `on:`, not `operation:`", x.ID)
		}
	case Measured, Informal, Obligation:
	default:
		l.errf(where, "rule %s: unknown kind %q", x.ID, x.Kind)
	}
	if x.Assume && x.On == "" {
		l.errf(where, "rule %s: `assume:` is for a fact about the situation a step runs in; name the step with `on:` (a fact about every state is a plain invariant)", x.ID)
	}
	if x.On != "" && x.Kind != Invariant {
		l.errf(where, "rule %s: `on:` is for invariants about a step; permissions and preconditions name `operation:`", x.ID)
	}
	var err error
	if d.When != "" {
		if x.When, err = ParseExpr(d.When); err != nil {
			l.errf(where, "rule %s: when: %v", x.ID, err)
		}
	}
	if d.Assert != "" {
		if x.Assert, err = ParseExpr(d.Assert); err != nil {
			l.errf(where, "rule %s: assert: %v", x.ID, err)
		}
	}
	l.reg.Rules = append(l.reg.Rules, x)
}

func (l *loader) decision(d *rawDoc, where string) {
	dec := &Decision{ID: d.ID, Rationale: strings.TrimSpace(d.Rationale), File: where}
	for _, s := range d.Resolves {
		dec.Resolves = append(dec.Resolves, l.pin(s, where))
	}
	for _, s := range d.Retire {
		dec.Retire = append(dec.Retire, l.pin(s, where))
	}
	if s := d.Supersede; s != nil {
		dec.Supersede = &Supersede{Winner: s.Winner, Loser: s.Loser}
		if s.When != "" {
			w, err := ParseExpr(s.When)
			if err != nil {
				l.errf(where, "decision %s: supersede.when: %v", d.ID, err)
			}
			dec.Supersede.When = w
		}
	}
	l.reg.Decisions = append(l.reg.Decisions, dec)
}

func (l *loader) openConflict(d *rawDoc, where string) {
	o := &OpenConflict{ID: d.ID, Response: d.Response, Situation: strings.TrimSpace(d.Situation),
		AcknowledgedBy: d.AcknowledgedBy, Since: d.Since, ClosedBy: d.ClosedBy, File: where}
	switch d.Status {
	case "", "open":
	case "closed":
		o.Closed = true
	default:
		l.errf(where, "open conflict %s: status must be open or closed (stale is derived from the pinned versions)", d.ID)
	}
	for _, s := range d.Rules {
		if !strings.Contains(s, "@") {
			o.Rules = append(o.Rules, Pin{Rule: s}) // an operation's from or effect; checked in finish
			continue
		}
		o.Rules = append(o.Rules, l.pin(s, where))
	}
	for _, x := range l.reg.Open {
		if x.ID == o.ID {
			l.errf(where, "open conflict %s declared twice", o.ID)
			return
		}
	}
	l.reg.Open = append(l.reg.Open, o)
}

func (l *loader) pin(s, where string) Pin {
	id, v, ok := strings.Cut(s, "@")
	if !ok {
		l.errf(where, "%q: pin rule versions as ID@version (docs/04: decisions)", s)
		return Pin{Rule: s}
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.errf(where, "%q: bad version", s)
	}
	return Pin{Rule: id, Version: n}
}

// finish wires inverse relations, then resolves every rule and decision
// against the full symbol table.
func (l *loader) finish() {
	// Entities, then their extensions, then operations: each may sit in any file.
	for _, x := range l.extends {
		name := strings.TrimPrefix(x.d.Extend, "platform.")
		if l.reg.Entities[name] == nil {
			l.errf(x.where, "extend %q: no such entity", x.d.Extend)
			continue
		}
		l.entity(name, x.d, x.where, true)
	}
	for _, d := range l.opDocs {
		ent, _, _ := strings.Cut(d.name, ".")
		e := l.reg.Entities[ent]
		switch {
		case e == nil:
			l.errf(d.where, "operation %s: no entity %s", d.name, ent)
		case l.reg.Operations[d.name] != nil:
			l.errf(d.where, "operation %s declared twice", d.name)
		default:
			l.reg.Operations[d.name] = &Operation{Entity: e, Name: d.name, Default: d.def, File: d.where}
		}
	}
	// A step decides its result.
	for _, a := range l.reg.Entities["Result"].Attrs {
		a.Controller = System
	}
	for _, inv := range l.inverses {
		to := l.reg.Entities[inv.to]
		if to == nil {
			l.errf(inv.where, "relation %s -> %s: unknown entity %s", inv.from, inv.to, inv.to)
			continue
		}
		if to.Relations[inv.name] == nil {
			to.Relations[inv.name] = &Relation{Name: inv.name, To: inv.from, Inverse: inv.rel}
		} else if to.Relations[inv.name].Inverse == "" {
			to.Relations[inv.name].Inverse = inv.rel
		}
	}
	l.instanceRelations()
	for _, e := range l.reg.Entities {
		for _, a := range e.Attrs {
			for _, x := range []*Attr{a, a.Elem, a.Key} {
				if x != nil && x.Kind == Enum && len(x.Values) == 0 && e.Context != "platform" {
					l.errf(e.Name, "%s: enum with no values", a.QName())
				}
			}
		}
	}
	l.operations()

	for _, x := range l.reg.Rules {
		if op := x.Op(); op != "" && OpPattern(op) {
			if ent := strings.TrimSuffix(op, ".*"); op != "*" && l.reg.Entities[ent] == nil {
				l.errf(x.File, "rule %s: operation pattern %q names no entity", x.ID, op)
				continue
			}
		} else if op != "" && l.reg.Operations[op] == nil {
			l.errf(x.File, "rule %s: unknown operation %q (declare it with `operation: %s`)", x.ID, op, op)
			continue
		}
		subject, err := l.reg.SubjectOf(x)
		if err != nil {
			l.errs = append(l.errs, fmt.Errorf("%s: %w", x.File, err))
			continue
		}
		if x.Kind == Derivation {
			if x.Live() { // a draft or retired rule declares nothing yet
				l.declaredDefault(x, subject)
			}
			continue
		}
		for _, e := range []Expr{x.When, x.Assert} {
			if e == nil {
				continue
			}
			l.history(x, e)
			l.bounds(x, e, subject)
			for _, p := range Paths(e) {
				ref, err := l.reg.ResolveRef(p, subject)
				if err != nil {
					l.errf(x.File, "rule %s: %v", x.ID, err)
				} else if a := ref.Attr; a != nil && a.Entity.Name == "Request" && x.Op() == "" {
					l.errf(x.File, "rule %s: %s exists only while an operation is handled; name the operation with `on:`", x.ID, p)
				}
			}
		}
	}
	l.listsFitInstances() // after bounds raised the instances to fit counts
	for _, o := range l.reg.Open {
		for _, p := range o.Rules {
			r := l.reg.Rule(p.Rule)
			switch {
			case p.Version == 0:
				i := strings.LastIndexByte(p.Rule, '.')
				if r != nil || i < 0 || l.reg.Operations[p.Rule[:i]] == nil || (p.Rule[i+1:] != "from" && p.Rule[i+1:] != "effect") {
					l.errf(o.File, "open conflict %s: %q: pin rules as ID@version; only an operation's from or effect has none", o.ID, p.Rule)
				}
			case r == nil:
				l.errf(o.File, "open conflict %s: unknown rule %s", o.ID, p.Rule)
			case r.Version != p.Version:
				// Unlike a stale decision this is no error: the conflict blocks
				// again if it remains, and the record closes if it is gone.
				o.Stale = true
			}
		}
	}
	for _, d := range l.reg.Decisions {
		for _, p := range append(append([]Pin{}, d.Resolves...), d.Retire...) {
			r := l.reg.Rule(p.Rule)
			switch {
			case r == nil:
				l.errf(d.File, "decision %s: unknown rule %s", d.ID, p.Rule)
			case r.Version != p.Version:
				// docs/04: an edited rule makes the decision stale and re-opens the conflict.
				l.errf(d.File, "decision %s is stale: pins %s but %s is at version %d", d.ID, p, r.ID, r.Version)
			}
		}
		if s := d.Supersede; s != nil {
			for _, id := range []string{s.Winner, s.Loser} {
				if l.reg.Rule(id) == nil {
					l.errf(d.File, "decision %s: unknown rule %s", d.ID, id)
				}
			}
			if r := l.reg.Rule(s.Loser); r != nil && r.Kind == Precondition {
				l.errf(d.File, "decision %s: preconditions cannot be superseded (D-024)", d.ID)
			}
			if s.When != nil {
				for _, p := range Paths(s.When) {
					if _, err := l.reg.Resolve(p, nil); err != nil {
						l.errf(d.File, "decision %s: %v", d.ID, err)
					}
				}
			}
		}
	}
}

// history creates the history attributes of every operation e reads with
// ran, runs or lastRun (D-051). Running the operation writes them.
func (l *loader) history(x *Rule, e Expr) {
	var walk func(Expr)
	walk = func(e Expr) {
		switch e := e.(type) {
		case *Binary:
			walk(e.L)
			walk(e.R)
		case *Not:
			walk(e.X)
		case *Scale:
			walk(e.X)
		case *Set:
			for _, y := range e.Elems {
				walk(y)
			}
		case *Index:
			walk(e.X)
			walk(e.K)
		case *Quant:
			walk(e.Coll)
			walk(e.Body)
		case *Call:
			if !HistoryFunctions[e.Fn] {
				for _, y := range e.Args {
					walk(y)
				}
				return
			}
			p, ok := e.Args[0].(*Path)
			op := l.reg.Operations[strings.Join(pathSegs(p, ok), ".")]
			if op == nil {
				l.errf(x.File, "rule %s: %s needs an operation name, Entity.operation (got %s)", x.ID, e.Fn, e.Args[0])
				return
			}
			if op.History == nil {
				_, short, _ := strings.Cut(op.Name, ".")
				h := &History{
					Ran:  &Attr{Entity: op.Entity, Name: "ran(" + short + ")", Kind: Bool},
					Runs: &Attr{Entity: op.Entity, Name: "runs(" + short + ")", Kind: Int, HasRange: true, Max: MaxRuns},
					Last: &Attr{Entity: op.Entity, Name: "lastRun(" + short + ")", Kind: Time, Optional: true},
				}
				op.History = h
				op.Writes = append(op.Writes, h.Ran, h.Runs, h.Last)
				for _, a := range []*Attr{h.Ran, h.Runs, h.Last} {
					a.Controller = System
				}
			}
		}
	}
	walk(e)
}

// bounds raises the instance bound of a relation to many so that a count a
// rule compares with fits: count(r in self.roles: ...) = 18 needs at least
// 19 instances to tell 18 from more. Bounds stop at MaxInstances.
func (l *loader) bounds(x *Rule, e Expr, subject *Entity) {
	var walk func(Expr)
	need := func(q Expr, n int) {
		// len(list or set) = n needs room for n elements.
		if c, ok := q.(*Call); ok && c.Fn == "len" {
			if p, ok := c.Args[0].(*Path); ok {
				if a, err := l.reg.Resolve(p, subject); err == nil && (a.Kind == List || a.Bounded()) {
					want := min(max(n, 0), MaxInstances)
					if n > MaxInstances {
						l.reg.Notes = append(l.reg.Notes, fmt.Sprintf("rule %s compares len(%s) with %d; it is checked up to %d elements", x.ID, a.QName(), n, MaxInstances))
					}
					if a.MaxLen < want {
						l.reg.Notes = append(l.reg.Notes, fmt.Sprintf("%s is checked up to %d elements (was %d) to fit rule %s", a.QName(), want, a.MaxLen, x.ID))
						a.MaxLen = want
					}
				}
			}
			return
		}
		c, ok := q.(*Quant)
		if !ok || c.Op != "count" {
			return
		}
		p, ok := c.Coll.(*Path)
		if !ok {
			return
		}
		ref, err := l.reg.ResolveRef(p, subject)
		if err != nil || ref.Many == nil {
			return
		}
		want := min(n+1, MaxInstances)
		if n+1 > MaxInstances {
			l.reg.Notes = append(l.reg.Notes, fmt.Sprintf("rule %s compares a count of %s.%s with %d; it is checked over at most %d instances", x.ID, ref.From.Name, ref.Many.Name, n, MaxInstances))
		}
		if ref.Many.Max < want {
			l.reg.Notes = append(l.reg.Notes, fmt.Sprintf("%s.%s is checked over %d instances (was %d) to fit rule %s's count", ref.From.Name, ref.Many.Name, want, ref.Many.Max, x.ID))
			ref.Many.Max = want
		}
	}
	walk = func(e Expr) {
		switch e := e.(type) {
		case *Binary:
			if cmpOps[e.Op] {
				if n, ok := e.R.(*IntLit); ok {
					need(e.L, n.V)
				}
				if n, ok := e.L.(*IntLit); ok {
					need(e.R, n.V)
				}
			}
			walk(e.L)
			walk(e.R)
		case *Not:
			walk(e.X)
		case *Quant:
			walk(e.Body)
		}
	}
	walk(e)
}

// MaxRuns bounds how often the checker counts an operation as having run.
const MaxRuns = 1000000

func pathSegs(p *Path, ok bool) []string {
	if !ok {
		return nil
	}
	return p.Segs
}

// declaredDefault handles the one derivation form supported so far:
// `path.default = literal`, which records a configuration default.
func (l *loader) declaredDefault(x *Rule, subject *Entity) {
	b, ok := x.Assert.(*Binary)
	var p *Path
	if ok {
		p, _ = b.L.(*Path)
	}
	if !ok || b.Op != "=" || p == nil || p.Segs[len(p.Segs)-1] != "default" {
		l.errf(x.File, "rule %s: only `<attribute>.default = <literal>` derivations are supported yet", x.ID)
		return
	}
	attrPath := &Path{Segs: p.Segs[:len(p.Segs)-1]}
	a, err := l.reg.Resolve(attrPath, subject)
	if err != nil {
		l.errf(x.File, "rule %s: %v", x.ID, err)
		return
	}
	// Two statements may declare the same default. Different defaults for one
	// setting cannot both hold; that is a conflict between the statements for
	// the checker to report, not an error in the registry.
	for _, d := range a.Defaults {
		if d.Value.String() == b.R.String() {
			return
		}
	}
	a.Defaults = append(a.Defaults, DeclaredDefault{Value: b.R, By: x.ID})
	if a.Default == nil {
		a.Default, a.DefaultBy = b.R, x.ID
	}
}

// operations resolves writes and effects, then derives which attributes the
// system controls.
func (l *loader) operations() {
	for _, d := range l.opDocs {
		op := l.reg.Operations[d.name]
		if op == nil || op.File != d.where {
			continue // reported when it was created
		}
		l.writes(op, d)
		keys := make([]string, 0, len(d.effect))
		for k := range d.effect {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			a, err := l.reg.Resolve(&Path{Segs: append([]string{"self"}, strings.Split(k, ".")...)}, op.Entity)
			if err != nil {
				l.errf(d.where, "operation %s effect: %v", d.name, err)
				continue
			}
			if !op.WritesAttr(a) {
				l.errf(d.where, "operation %s effect sets %s, which it does not list in writes", d.name, a.QName())
				continue
			}
			v, err := ParseExpr(fmt.Sprint(d.effect[k]))
			if err != nil {
				l.errf(d.where, "operation %s effect %s: %v", d.name, k, err)
				continue
			}
			op.Effect = append(op.Effect, Assignment{Attr: a, Value: v})
		}
	}
	for _, d := range l.opExtends {
		if op := l.reg.Operations[d.name]; op == nil {
			l.errf(d.where, "extend %s: no such operation", d.name)
		} else {
			l.writes(op, d)
		}
	}
	// A value the world sets may be written only as a declared reset, so the
	// step cannot choose it to avoid a rule (ISSUES 19).
	for _, op := range l.reg.Operations {
		for _, a := range op.Writes {
			if a.WorldSet && !slices.ContainsFunc(op.Effect, func(e Assignment) bool { return e.Attr == a }) {
				l.errf(op.File, "operation %s writes %s, which the world sets (setBy: world): a step may only reset it to a declared value, in effect; otherwise drop it from writes, and rules read it as part of the situation", op.Name, a.QName())
			}
		}
	}
	l.creations()
	l.stateMachines()
	l.requirementOutputs()
	for _, op := range l.reg.Operations {
		for _, a := range op.Writes {
			if a.WorldSet {
				continue // the world's, reset by the step's effect
			}
			a.Controller = System
			if !a.maxLenDeclared && a.MaxLen == DefaultMaxLen {
				// A list the system builds can hold what several inputs
				// give it (an id, then each of three keys, then three
				// defaults), so it is checked longer than an input list.
				a.MaxLen = ResponseMaxLen
			}
		}
	}
}

// listsFitInstances checks a list that declares no maxLength at least as
// long as the most instances any relation to many is checked over: "the
// overdue list holds every overdue tool" needs room for every tool. A count
// rule had raised the tools to 4 while the list stayed at 3, and the rule
// read as contradicting itself however it was worded (a developer trial,
// 2026-11-03).
func (l *loader) listsFitInstances() {
	most := 0
	for _, e := range l.reg.Entities {
		for _, r := range e.Relations {
			if r.Many {
				most = max(most, r.Max)
			}
		}
	}
	for _, e := range l.reg.Entities {
		for _, a := range e.Attrs {
			if (a.Kind == List || a.Bounded()) && !a.maxLenDeclared && a.MaxLen < most {
				a.MaxLen = most
			}
		}
	}
}

// requirementOutputs makes the system control what a spec statement's
// state invariant obliges, even when no operation writes it: "the system
// MUST warn before shutdown" and "MUST NOT show warnings" are requirements
// on the system, not assumptions about the world (D-049), so they conflict
// instead of together ruling out every situation near a shutdown. Values
// the world sets, requests and actors stay the environment's, and so does
// what a rule only reads (in "when", or before "implies").
func (l *loader) requirementOutputs() {
	for _, x := range l.reg.Rules {
		if !x.Live() || x.StatementID == "" || x.Kind != Invariant || x.On != "" || x.Assert == nil {
			continue
		}
		subject, err := l.reg.SubjectOf(x)
		if err != nil {
			continue
		}
		for _, part := range obliged(x.Assert) {
			for _, a := range l.reg.Reads(part, subject) {
				if a.Controller == Environment && !a.WorldSet && a.Entity != nil && a.Entity.Name != "Request" && a.Entity.Name != "Actor" {
					a.Controller = System
				}
			}
		}
	}
}

// obliged returns the parts of an assert that state what must hold: the
// consequent of an implication, both sides of a conjunction or
// disjunction, and every other expression whole.
func obliged(e Expr) []Expr {
	switch e := e.(type) {
	case *Binary:
		switch e.Op {
		case "implies":
			return obliged(e.R)
		case "and", "or":
			return append(obliged(e.L), obliged(e.R)...)
		}
	case *Not:
		return obliged(e.X)
	}
	return []Expr{e}
}

// stateMachines turns each transition into its operation's start states and
// effect (docs/04): `{from: running, to: stopped, via: stop}` means
// Workspace.stop runs only from running and leaves the workspace stopped.
func (l *loader) stateMachines() {
	for _, m := range l.machines {
		e := l.reg.Entities[m.entity]
		field := e.Attrs[m.sm.Field]
		if field == nil || field.Kind != Enum {
			l.errf(m.where, "%s stateMachine: field %q must be an enum attribute", m.entity, m.sm.Field)
			continue
		}
		for _, t := range m.sm.Transitions {
			name := m.entity + "." + t.Via
			op := l.reg.Operations[name]
			if op == nil {
				l.errf(m.where, "%s stateMachine: transition via %s needs `operation: %s`", m.entity, t.Via, name)
				continue
			}
			var from []string
			switch t.From.Kind {
			case yaml.ScalarNode:
				from = []string{t.From.Value}
			case yaml.SequenceNode:
				if err := t.From.Decode(&from); err != nil {
					l.errf(m.where, "%s: %v", name, err)
				}
			}
			for _, v := range append(append([]string{}, from...), t.To) {
				if !slices.Contains(field.Values, v) {
					l.errf(m.where, "%s stateMachine: %q is not a value of %s", m.entity, v, field.QName())
				}
			}
			if op.From != nil {
				l.errf(m.where, "%s stateMachine: %s appears in more than one transition; list every start state in one `from`", m.entity, name)
				continue
			}
			if !op.WritesAttr(field) {
				op.Writes = append(op.Writes, field)
			}
			op.From = &Assignment{Attr: field, Values: from}
			op.Effect = append(op.Effect, Assignment{Attr: field, Value: &Ident{Name: t.To}})
		}
	}
}

// creations makes each operation that creates an entity write every
// attribute of it the world does not set, and every optional relation's
// presence: a new instance carries nothing over from an earlier state.
func (l *loader) creations() {
	for _, c := range l.creators {
		e := l.reg.Entities[c.entity]
		if e == nil {
			continue // reported with the entity
		}
		for _, name := range c.ops {
			op := l.reg.Operations[name]
			switch {
			case op == nil:
				l.errf(c.where, "%s createdBy %s: no such operation", c.entity, name)
				continue
			case op.Entity != e:
				l.errf(c.where, "%s createdBy %s: the operation belongs to %s", c.entity, name, op.Entity.Name)
				continue
			}
			if !slices.Contains(e.CreatedBy, name) {
				e.CreatedBy = append(e.CreatedBy, name)
			}
			for _, an := range e.AttrNames() {
				if a := e.Attrs[an]; !a.WorldSet && !op.WritesAttr(a) {
					op.Writes = append(op.Writes, a)
				}
			}
			rels := make([]string, 0, len(e.Relations))
			for rn := range e.Relations {
				rels = append(rels, rn)
			}
			sort.Strings(rels)
			for _, rn := range rels {
				if r := e.Relations[rn]; r.Set != nil && !op.WritesAttr(r.Set) {
					op.Writes = append(op.Writes, r.Set)
				}
			}
		}
		sort.Strings(e.CreatedBy)
	}
}

// writes resolves an operation document's writes onto the operation.
func (l *loader) writes(op *Operation, d opDoc) {
	for _, w := range d.writes {
		ent, attr, ok := strings.Cut(w, ".")
		e := l.reg.Entities[ent]
		if !ok || e == nil {
			l.errf(d.where, "operation %s writes %q: not a declared Entity.attribute", d.name, w)
			continue
		}
		if e.Name == "Request" || e.Name == "Actor" {
			l.errf(d.where, "operation %s writes %s: requests and actors are inputs, not state", d.name, w)
			continue
		}
		// Entity.attribute, a record field (Entity.record.field), or an
		// optional relation, whose presence the operation sets.
		p := &Path{Segs: append([]string{"self"}, strings.Split(attr, ".")...)}
		a, err := l.reg.Resolve(p, e)
		if ref, rerr := l.reg.ResolveRef(p, e); err != nil && rerr == nil && ref.Rel != nil {
			a, err = ref.Rel.Set, nil
		}
		if err != nil {
			l.errf(d.where, "operation %s writes %q: not a declared Entity.attribute", d.name, w)
			continue
		}
		if !op.WritesAttr(a) {
			op.Writes = append(op.Writes, a)
		}
	}
}

// instanceRelations adds, to each entity a rule reads with instances(E),
// the relation to its other instances: up to DefaultMaxInstances (raised to
// fit a count a rule compares, like any relation to many).
func (l *loader) instanceRelations() {
	for _, x := range l.reg.Rules {
		for _, e := range []Expr{x.When, x.Assert} {
			if e == nil {
				continue
			}
			for _, p := range Paths(e) {
				if len(p.Segs) != 2 || p.Segs[1] != InstancesRelation {
					continue
				}
				ent := l.reg.rootEntity(p.Segs[0])
				if ent == nil {
					l.errf(x.File, "rule %s: %s: no such entity", x.ID, p)
					continue
				}
				if ent.Relations[InstancesRelation] == nil {
					ent.Relations[InstancesRelation] = &Relation{Name: InstancesRelation, To: ent.Name, Many: true, Max: DefaultMaxInstances}
				}
			}
		}
	}
}
