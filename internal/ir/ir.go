// Package ir holds the Stele registry model (docs/04-data-model.md) and loads
// it from YAML.
package ir

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Registry is everything loaded from one registry directory.
// Now is the clock: the time at which the situation is seen and an
// operation runs (D-051). The world sets it.
var Now = &Attr{Entity: &Entity{Name: "Clock", Attrs: map[string]*Attr{}, Relations: map[string]*Relation{}}, Name: "now", Kind: Time, WorldSet: true}

// MaxInstances caps how far a relation's bound is raised to fit a count.
const MaxInstances = 64

type Registry struct {
	// Notes are facts about how the registry is checked, for reports: a
	// relation's instance bound raised to fit a count, for example.
	Notes      []string
	Contexts   map[string]*Context
	Entities   map[string]*Entity    // by entity name, e.g. "Workspace"
	Operations map[string]*Operation // by qualified name, e.g. "ChatSession.sendFollowUp"
	Rules      []*Rule
	Decisions  []*Decision
	Open       []*OpenConflict
	// Lessons are what the owner taught Stele about this project, read by
	// every translation and coverage check (D-063).
	Lessons []*Lesson
	// Unchecked are the steps the solver could not decide that the owner
	// approved changes to knowing they stay unchecked, by step.
	Unchecked map[string]*UncheckedStep
}

// Lesson kinds.
const (
	LessonTerm    = "term"    // two names for one thing, or an entity identity
	LessonFact    = "fact"    // something true of the world in this project
	LessonReading = "reading" // how one requirement is to be read; always pinned
	LessonScope   = "scope"   // a project-wide reading preference
)

// Lesson is one thing the owner taught Stele about this project, saved in
// the registry (lessons/<id>.yaml) and committed with the specs.
type Lesson struct {
	ID   string
	Kind string
	Says string // the owner's words, as the translator should follow them
	// Statements pins the lesson to statements by the hash of their text;
	// empty means the whole project.
	Statements map[string]string
	Terms      []string // words for finding the statements it concerns
	From       string   // where it came from: the change and question
	By, At     string
	File       string
}

// Pinned reports whether the lesson is about particular statements.
func (l *Lesson) Pinned() bool { return len(l.Statements) > 0 }

// AppliesTo reports whether a pinned lesson applies to a statement as now
// written, and whether it is stale: pinned to the statement's earlier text.
// A project-wide lesson applies everywhere; which ones a request needs is
// decided by relevance (internal/lesson).
func (l *Lesson) AppliesTo(stmt, textHash string) (applies, stale bool) {
	if !l.Pinned() {
		return true, false
	}
	h, ok := l.Statements[stmt]
	if !ok {
		return false, false
	}
	return h == textHash, h != textHash
}

// Context is a capability boundary and unit of ownership.
type Context struct {
	Name  string
	Owner string
}

// Entity is a typed thing with attributes and relations.
// DeclaredDefault is one declared default value of an attribute.
type DeclaredDefault struct {
	Value Expr
	By    string // the rule that declared it
}

type Entity struct {
	Name      string
	Context   string
	Attrs     map[string]*Attr
	Relations map[string]*Relation
	// CreatedBy names the operations that create the entity: they never run
	// on one that already exists, so they set every attribute the world does
	// not, and nothing of an earlier state carries over.
	CreatedBy []string
}

// AttrNames returns the entity's attribute names in sorted order.
func (e *Entity) AttrNames() []string {
	names := make([]string, 0, len(e.Attrs))
	for n := range e.Attrs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Kind is an attribute's type.
type Kind int

const (
	Bool Kind = iota
	Int
	Enum
	String   // text, also bytes (as text)
	Decimal  // exact decimal numbers (SMT reals)
	Duration // milliseconds
	Time     // milliseconds since the Unix epoch, UTC
	List     // a sequence of Elem, checked up to MaxLen elements
	SetKind  // a set of Elem (Set is the set literal)
	Map      // Key to Elem, with the set of present keys
)

// KindNames are the type names the YAML format uses.
var KindNames = map[Kind]string{Bool: "bool", Int: "int", Enum: "enum", String: "string", Decimal: "decimal",
	Duration: "duration", Time: "time", List: "list", SetKind: "set", Map: "map"}

// Bounded reports whether a is a set checked as a bounded sequence of
// distinct elements: a set of strings, numbers or times. Sets of enum or
// bool values range over every value instead.
func (a *Attr) Bounded() bool {
	return a.Kind == SetKind && a.Elem != nil && a.Elem.Kind != Enum && a.Elem.Kind != Bool
}

// Numeric reports whether values of k are numbers.
func (k Kind) Numeric() bool { return k == Int || k == Decimal || k == Duration || k == Time }

// DefaultMaxLen bounds lists that declare no maxLength. ResponseMaxLen
// bounds those an operation writes: built from inputs, they can be longer.
const (
	DefaultMaxLen  = 3
	ResponseMaxLen = 8
)

// DefaultMaxInstances bounds relations to many that declare no max.
const DefaultMaxInstances = 3

// Controller says who decides an attribute's value, after Jackson and Zave's
// split between environment-controlled and machine-controlled phenomena. It is
// derived, not declared (D-042): an attribute some operation writes is
// system-controlled; every other attribute (clocks, counters, configuration)
// is set by the environment.
type Controller int

const (
	Environment Controller = iota // the situation: requests, clocks, counters, configuration
	System                        // written by some operation
)

// Attr is one attribute of an entity.
type Attr struct {
	Entity     *Entity
	Name       string
	Kind       Kind
	Values     []string // enum values
	HasRange   bool
	Min, Max   int    // int range
	RMin, RMax string // decimal range, as written
	// Elem is a list's or set's element type, or a map's value type; Key is a
	// map's key type. Both are attributes of the same entity that are not
	// members of it: they carry the element's kind, enum values and range.
	Elem, Key      *Attr
	MaxLen         int // lists: the longest list checked (DefaultMaxLen, or ResponseMaxLen when written, if not declared)
	maxLenDeclared bool
	Optional       bool // the attribute may have no value (none)
	Controller     Controller
	Default        Expr   // declared default, from a derivation `X.default = v`
	DefaultBy      string // rule that declared the default
	// Defaults lists each distinct declared default with the first rule that
	// declared it. More than one cannot all hold: the checker reports them.
	Defaults []DeclaredDefault
	Meaning  string // the fact in plain words, optional
	// WorldSet marks a value the world decides (time, what users or other
	// services do): no operation may write it, since the checker treats a
	// written value as the system's choice (ISSUES 19).
	WorldSet bool
}

// QName is the attribute's qualified name, e.g. "Workspace.status". A record
// field's entity is named Owner$record: Job$result.code.
func (a *Attr) QName() string { return a.Entity.Name + "." + a.Name }

// Path is the attribute as Entity.attribute, with a record field reached
// through its record: Job.result.code. Operations write attributes by it.
func (a *Attr) Path() string {
	// An optional relation's presence is written as the relation itself.
	return strings.ReplaceAll(a.Entity.Name, "$", ".") + "." + strings.TrimSuffix(a.Name, "?")
}

// Owner is the entity an attribute belongs to, through any records.
func (a *Attr) Owner() string {
	owner, _, _ := strings.Cut(a.Entity.Name, "$")
	return owner
}

// TypeString renders a's type as the format writes it: int 0..10,
// enum {a, b}, list<string> (up to 3), map<enum {gold, silver}, int>.
func (a *Attr) TypeString() string {
	s := ""
	switch a.Kind {
	case Int:
		s = "int"
		if a.HasRange {
			s = fmt.Sprintf("int %d..%d", a.Min, a.Max)
		}
	case Decimal:
		s = "decimal"
		if a.HasRange {
			s = fmt.Sprintf("decimal %s..%s", a.RMin, a.RMax)
		}
	case Enum:
		s = "enum {" + strings.Join(a.Values, ", ") + "}"
	case List:
		s = fmt.Sprintf("list<%s> (up to %d)", a.Elem.TypeString(), a.MaxLen)
	case SetKind:
		s = "set<" + a.Elem.TypeString() + ">"
	case Map:
		s = "map<" + a.Key.TypeString() + ", " + a.Elem.TypeString() + ">"
	default:
		s = KindNames[a.Kind]
	}
	if a.Optional {
		s = "optional " + s
	}
	return s
}

// Relation navigates from one entity to another.
type Relation struct {
	Name string
	To   string
	// Many relates to several instances, checked as Max instances other than
	// the one roots and self denote, ranged over with all, any and count.
	Many bool
	Max  int
	// Inverse is the relation back from To, when declared.
	Inverse string
	// Record marks the relation to a record attribute's fields: an entity of
	// its own, owned by this one.
	Record bool
	// Optional relations may lead nowhere; Set is the Bool that says whether
	// they do (isSet(self.project), self.project = none). Operations that
	// list the relation in writes change it.
	Optional bool
	Set      *Attr
}

// Operation is a state change on an entity: one step of the system, invoked
// by an actor (a user, a scheduler, another service). Attributes it does not
// write keep their value across the step (the frame).
type Operation struct {
	Entity *Entity
	Name   string // qualified, e.g. "ChatSession.sendFollowUp"
	// History holds the operation's history attributes, created when a rule
	// reads them: whether it has run, how often, and when it last ran.
	History *History
	Default string // allow | deny: whether it runs when no permission rule matches
	Writes  []*Attr
	Effect  []Assignment // unconditional results of running it
	From    *Assignment  // from a state machine: it runs only when Attr is one of From.Values
	File    string
}

// History attributes record an operation's past (D-051): Ran (bool), Runs
// (int) and Last (optional time, the clock when it last ran). Running the
// operation sets Ran, adds one to Runs and sets Last to now.
type History struct{ Ran, Runs, Last *Attr }

// Assignment sets an attribute to a literal value. For an operation's From,
// Values lists the states it may start from.
type Assignment struct {
	Attr   *Attr
	Value  Expr
	Values []string
}

// WritesAttr reports whether op writes a.
func (op *Operation) WritesAttr(a *Attr) bool {
	for _, w := range op.Writes {
		if w == a {
			return true
		}
	}
	return false
}

// Rule kinds (docs/05-ir-and-semantics.md#rule-kinds).
const (
	Permission   = "permission"
	Precondition = "precondition"
	Invariant    = "invariant"
	Derivation   = "derivation"
	Measured     = "measured"
	Informal     = "informal"
	// Obligation is a requirement about the code or the development process
	// (tests exist, CI runs, a function has a signature): checked against the
	// repository, not against other statements (D-049, D-052).
	Obligation = "obligation"
)

// Rule is a typed constraint on an operation or the state.
type Rule struct {
	ID        string
	Version   int
	Kind      string
	Effect    string // allow | deny, for permissions
	Operation string // qualified operation name, for permissions and preconditions
	On        string // for invariants about a step: the operation; when reads the state before it, assert the state after
	// Assume makes an invariant on a step a fact about the situation it
	// runs in: when and assert both read the state before it and the
	// request, and the check assumes it rather than requiring it (a
	// glossary's definition, ISSUES 62).
	Assume      bool
	Subject     string // entity that `self` denotes; inferred when empty
	When        Expr   // guard; nil means always
	Assert      Expr   // for invariants and derivations
	Status      string
	Source      string // doc reference
	StatementID string // current imported statement ID; independent of the rule's stable ID
	// Overrides names statements this rule takes precedence over where it
	// applies, because its own text says so ("regardless of --force",
	// "per FR-005, not when --dry-run is set"): an explicit precedence (D-052).
	Overrides     []string
	StatementHash string // latest imported text, independent of the original translation's TextHash
	Prose         string
	// Rationale says, for an informal or obligation reading, which part of
	// the statement it leaves to review and why.
	Rationale string
	// TextHash and Introduced identify the statement text the rule was
	// translated from, for adoption sync (docs/08 W5).
	TextHash   string
	Introduced string
	// ApprovedBy and ApprovedHash record the owner's approval of this
	// rule as a reading of its statement, pinned to the statement text it
	// was translated from (D-056, D-057).
	ApprovedBy   string
	ApprovedHash string
	// ApprovedReading is the ReadingHash of the rule the owner approved: a
	// rule read again from unchanged text needs approving again. Empty for
	// approvals recorded before readings were pinned.
	ApprovedReading string
	File            string
}

// Approved reports whether the owner approved the rule for the statement
// text it was translated from, as it now reads. A rule with no text hash
// (written by hand) is approved by name alone.
func (r *Rule) Approved() bool {
	return r.ApprovedBy != "" && r.ApprovedHash == r.TextHash && (r.ApprovedReading == "" || r.ApprovedReading == r.ReadingHash())
}

// Reading is the rule's meaning in one canonical line: everything the
// check reads, nothing about where it came from.
func (r *Rule) Reading() string {
	parts := []string{r.Kind, r.Effect, r.Operation, r.On, r.Subject, fmt.Sprint(r.Assume)}
	for _, e := range []Expr{r.When, r.Assert} {
		if e == nil {
			parts = append(parts, "")
		} else {
			parts = append(parts, e.String())
		}
	}
	parts = append(parts, strings.Join(r.Overrides, ","))
	return strings.Join(parts, "|")
}

// ReadingHash identifies Reading.
func (r *Rule) ReadingHash() string {
	h := sha256.Sum256([]byte(r.Reading()))
	return fmt.Sprintf("%x", h[:8])
}

// Op is the operation the rule is about: its `operation` for permissions and
// preconditions, its `on` for step invariants, or "".
func (r *Rule) Op() string {
	if r.Operation != "" {
		return r.Operation
	}
	return r.On
}

// OpPattern reports whether an operation name is a pattern: "*" for every
// operation, "Entity.*" for every operation of an entity.
func OpPattern(name string) bool { return name == "*" || strings.HasSuffix(name, ".*") }

// AppliesTo reports whether the rule is about operation op, by name or by
// pattern.
func (r *Rule) AppliesTo(op string) bool {
	name := r.Op()
	switch {
	case name == op:
		return true
	case name == "*":
		return op != ""
	case strings.HasSuffix(name, ".*"):
		return strings.HasPrefix(op, strings.TrimSuffix(name, "*"))
	}
	return false
}

// Statement is the statement the rule translates: its source statement ID,
// or its own ID without a trailing .N for a statement's further rules.
func (r *Rule) Statement() string {
	if r.StatementID != "" {
		return r.StatementID
	}
	if i := strings.LastIndexByte(r.ID, '.'); i > 0 && strings.Trim(r.ID[i+1:], "0123456789") == "" {
		return r.ID[:i]
	}
	return r.ID
}

// Overriders returns the live rules that override statement stmt.
func (r *Registry) Overriders(stmt string) []*Rule {
	var out []*Rule
	for _, x := range r.Rules {
		if x.Live() && slices.Contains(x.Overrides, stmt) {
			out = append(out, x)
		}
	}
	return out
}

// Live reports whether the rule takes part in checks (docs/05: active or deprecated).
func (r *Rule) Live() bool { return r.Status == "active" || r.Status == "deprecated" }

// Pin is a rule reference pinned to a version, written R12@3.
type Pin struct {
	Rule    string
	Version int
}

func (p Pin) String() string {
	if p.Version == 0 {
		return p.Rule
	}
	return fmt.Sprintf("%s@%d", p.Rule, p.Version)
}

// Decision is a precedence ruling between rules, or a retirement.
type Decision struct {
	ID        string
	Resolves  []Pin
	Supersede *Supersede
	Retire    []Pin
	Rationale string
	File      string
}

// OpenConflict is a conflict an adoption baseline tolerates until someone
// resolves it (D-044). It pins one minimal conflicting set, or the core of one
// dead response. The gate tolerates a conflict whose set is exactly Rules at
// the pinned versions, and nothing else.
type OpenConflict struct {
	ID string
	// Rules pins rule versions. An operation's own constraints (Op.from,
	// Op.effect) are not rules and carry no version.
	Rules          []Pin
	Response       string // for a dead response: the response no situation allows
	Situation      string
	AcknowledgedBy []string
	Since          string
	Closed         bool   // status: closed
	ClosedBy       string // decision or spec PR that closed it
	// Stale is set by the loader when a pinned rule is now at another
	// version: the record tolerates nothing until it is re-baselined or closed.
	Stale bool
	File  string
}

// RuleIDs returns the pinned rule IDs, sorted.
func (o *OpenConflict) RuleIDs() []string {
	out := make([]string, len(o.Rules))
	for i, p := range o.Rules {
		out[i] = p.Rule
	}
	sort.Strings(out)
	return out
}

// Supersede removes the loser rule wherever When holds.
type Supersede struct {
	Winner, Loser string
	When          Expr
}

// Rule returns the rule with the given ID, or nil.
func (r *Registry) Rule(id string) *Rule {
	for _, x := range r.Rules {
		if x.ID == id {
			return x
		}
	}
	return nil
}

// Root names an entity by its lower-camel name: `config` is Config,
// `chatSession` is ChatSession. Checks run over one instance of each entity
// (stele-research docs/HANDOFF.md#go-core), so a root names that instance.
func (r *Registry) rootEntity(root string) *Entity {
	switch root {
	case "request":
		return r.Entities["Request"]
	case "actor":
		return r.Entities["Actor"]
	}
	for name, e := range r.Entities {
		if lowerFirst(name) == root {
			return e
		}
	}
	return nil
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// Ref is what a path denotes: an attribute, or a relation to many.
type Ref struct {
	Attr *Attr
	// Rel is set for a path ending at an optional relation (isSet, none).
	Rel *Relation
	// Many is set for a path ending at a relation to many, with the entity it
	// leaves from.
	Many *Relation
	From *Entity
}

// ResolveRef is Resolve, also accepting a path that ends at a relation to
// many (the collection of a quantifier).
func (r *Registry) ResolveRef(p *Path, subject *Entity) (Ref, error) {
	if len(p.Segs) > 1 {
		head := &Path{Segs: p.Segs[:len(p.Segs)-1]}
		if e, err := r.entityAt(head, subject); err == nil {
			if rel := e.Relations[p.Segs[len(p.Segs)-1]]; rel != nil && rel.Many {
				return Ref{Many: rel, From: e}, nil
			}
		}
	}
	if len(p.Segs) > 1 {
		head := &Path{Segs: p.Segs[:len(p.Segs)-1]}
		if e, err := r.entityAt(head, subject); err == nil {
			if rel := e.Relations[p.Segs[len(p.Segs)-1]]; rel != nil && rel.Optional {
				return Ref{Rel: rel, From: e}, nil
			}
		}
	}
	a, err := r.Resolve(p, subject)
	return Ref{Attr: a}, err
}

// entityAt returns the entity a path of relations reaches.
func (r *Registry) entityAt(p *Path, subject *Entity) (*Entity, error) {
	var cur *Entity
	switch root := p.Segs[0]; root {
	case "self":
		cur = subject
	default:
		cur = r.rootEntity(root)
	}
	if cur == nil {
		return nil, fmt.Errorf("%s: no entity", p)
	}
	for _, s := range p.Segs[1:] {
		rel := cur.Relations[s]
		if rel == nil || rel.Many {
			return nil, fmt.Errorf("%s: no single relation %s", p, s)
		}
		cur = r.Entities[rel.To]
	}
	return cur, nil
}

// Resolve maps a path to the attribute it denotes. subject is the entity that
// `self` stands for, or nil when the rule has none.
func (r *Registry) Resolve(p *Path, subject *Entity) (*Attr, error) {
	var cur *Entity
	root := p.Segs[0]
	switch root {
	case "self":
		if subject == nil {
			return nil, fmt.Errorf("%s: `self` has no subject here", p)
		}
		cur = subject
	case "it":
		return nil, fmt.Errorf("%s: `it` is only valid inside an aggregate", p)
	default:
		cur = r.rootEntity(root)
		if cur == nil {
			return nil, fmt.Errorf("%s: unknown root %q (use self, actor, request, or an entity name such as %s)", p, root, r.exampleRoot())
		}
	}
	segs := p.Segs[1:]
	if len(segs) == 0 {
		return nil, fmt.Errorf("%s: a path must end at an attribute", p)
	}
	for i, s := range segs {
		last := i == len(segs)-1
		if a, ok := cur.Attrs[s]; ok {
			if !last {
				return nil, fmt.Errorf("%s: %s.%s is an attribute, not a relation", p, cur.Name, s)
			}
			return a, nil
		}
		if rel, ok := cur.Relations[s]; ok {
			if rel.Many {
				return nil, fmt.Errorf("%s: %s.%s relates to many %s; range over it with all, any or count, e.g. all(x in %s: x.<attribute> ...)", p, cur.Name, s, rel.To, strings.Join(p.Segs[:i+2], "."))
			}
			if last {
				return nil, fmt.Errorf("%s: path ends at relation %s.%s; name an attribute of %s", p, cur.Name, s, rel.To)
			}
			cur = r.Entities[rel.To]
			continue
		}
		return nil, fmt.Errorf("%s: %s has no attribute or relation %q", p, cur.Name, s)
	}
	panic("unreachable")
}

func (r *Registry) exampleRoot() string {
	names := make([]string, 0, len(r.Entities))
	for n := range r.Entities {
		if n != "Actor" && n != "Request" && n != "Result" {
			names = append(names, lowerFirst(n))
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "config"
	}
	return names[0]
}

// SubjectOf returns the entity `self` denotes in rule x: its declared subject,
// its operation's entity, or the one entity that has every first segment of
// its `self.` paths. It returns nil when the rule never mentions `self`.
func (r *Registry) SubjectOf(x *Rule) (*Entity, error) {
	if x.Subject != "" {
		e := r.Entities[x.Subject]
		if e == nil {
			return nil, fmt.Errorf("rule %s: unknown subject %q", x.ID, x.Subject)
		}
		return e, nil
	}
	if name := x.Op(); name == "*" {
		return nil, nil // every operation: self differs per operation, so rules name roots
	} else if strings.HasSuffix(name, ".*") {
		e := r.Entities[strings.TrimSuffix(name, ".*")]
		if e == nil {
			return nil, fmt.Errorf("rule %s: unknown entity in %q", x.ID, name)
		}
		return e, nil
	} else if name != "" {
		op := r.Operations[name]
		if op == nil {
			return nil, fmt.Errorf("rule %s: unknown operation %q (declare it with `operation: %s`)", x.ID, name, name)
		}
		return op.Entity, nil
	}
	var firsts []string
	for _, e := range []Expr{x.When, x.Assert} {
		if e == nil {
			continue
		}
		for _, p := range Paths(e) {
			if p.Segs[0] == "self" && len(p.Segs) > 1 {
				firsts = append(firsts, p.Segs[1])
			}
		}
	}
	if len(firsts) == 0 {
		return nil, nil
	}
	var found []*Entity
	for _, e := range r.Entities {
		ok := true
		for _, s := range firsts {
			_, a := e.Attrs[s]
			_, rel := e.Relations[s]
			if !a && !rel {
				ok = false
				break
			}
		}
		if ok {
			found = append(found, e)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return nil, fmt.Errorf("rule %s: no entity has all of self.{%s}; add `subject:`", x.ID, strings.Join(firsts, ", "))
	default:
		names := make([]string, len(found))
		for i, e := range found {
			names[i] = e.Name
		}
		sort.Strings(names)
		return nil, fmt.Errorf("rule %s: `self` could be any of %s; add `subject:`", x.ID, strings.Join(names, ", "))
	}
}

// Reads returns every attribute e reads, including those reached through a
// quantifier's variable (w.status in all(w in node.workspaces: ...)), in
// order of appearance. Paths that do not resolve are skipped.
func (r *Registry) Reads(e Expr, subject *Entity) []*Attr {
	var out []*Attr
	var walk func(Expr, map[string]*Entity)
	walk = func(e Expr, bound map[string]*Entity) {
		switch e := e.(type) {
		case *Binary:
			walk(e.L, bound)
			walk(e.R, bound)
		case *Not:
			walk(e.X, bound)
		case *Scale:
			walk(e.X, bound)
		case *Set:
			for _, x := range e.Elems {
				walk(x, bound)
			}
		case *ListLit:
			for _, x := range e.Elems {
				walk(x, bound)
			}
		case *Call:
			for _, x := range e.Args {
				walk(x, bound)
			}
		case *Index:
			walk(e.X, bound)
			walk(e.K, bound)
		case *Quant:
			walk(e.Coll, bound)
			inner := map[string]*Entity{}
			for k, v := range bound {
				inner[k] = v
			}
			inner[e.Var] = nil // an element value
			if p, ok := e.Coll.(*Path); ok {
				if ref, err := r.refIn(p, subject, bound); err == nil && ref.Many != nil {
					inner[e.Var] = r.Entities[ref.Many.To]
				}
			}
			walk(e.Body, inner)
		case *Path:
			if ref, err := r.refIn(e, subject, bound); err == nil && ref.Attr != nil {
				out = append(out, ref.Attr)
			} else if err == nil && ref.Rel != nil {
				out = append(out, ref.Rel.Set)
			}
		}
	}
	walk(e, map[string]*Entity{})
	return out
}

// refIn resolves p where some roots are quantifier variables bound to
// entities (or to element values, nil).
func (r *Registry) refIn(p *Path, subject *Entity, bound map[string]*Entity) (Ref, error) {
	if e, ok := bound[p.Segs[0]]; ok {
		if e == nil || len(p.Segs) == 1 {
			return Ref{}, fmt.Errorf("%s: an element value", p)
		}
		return r.ResolveRef(&Path{Segs: append([]string{"self"}, p.Segs[1:]...)}, e)
	}
	return r.ResolveRef(p, subject)
}

// UncheckedStep records that the owner approved rules on a step the solver
// could not decide, knowing that step is not checked: the gate accepts a
// check left undecided only there.
type UncheckedStep struct {
	Step string
	By   string
	At   string
	File string
}
