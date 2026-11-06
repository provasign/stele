package ir

import (
	"regexp"
	"slices"
)

// otherInstance names the instance a lifted invariant ranges over.
const otherInstance = "otherInstance"

// ForOtherInstances restates a state invariant about one instance of an
// entity over the other instances rules range over with instances(E):
// all(otherInstance in instances(E): when implies assert), self renamed.
// Those instances were otherwise in no state the invariants describe:
// "every tool's id is positive" held for the tool a step acts on and not
// for the tools a uniqueness or overdue rule counts (ISSUES 81). Only
// invariants that read the instance's own attributes, and quantify over
// nothing or over the instances of other entities, are restated: one about
// every other instance of its own entity would compare an instance with
// itself. Over another entity's instances it holds as written, or weaker
// in a step whose own instance those leave out: "a member on the waitlist
// holds no plot" kept the first member waiting from holding a plot already
// when a plot was released (a developer trial, 2026-11-05). It is nil when
// the entity has no such relation.
func (r *Registry) ForOtherInstances(x *Rule) *Rule {
	if x.Kind != Invariant || x.On != "" || x.Assert == nil {
		return nil
	}
	subject, err := r.SubjectOf(x)
	if err != nil || subject == nil || subject.Relations[InstancesRelation] == nil {
		return nil
	}
	for _, e := range []Expr{x.When, x.Assert} {
		if e == nil {
			continue
		}
		if quantifiesOver(e, subject) {
			return nil
		}
		for _, p := range Paths(e) {
			switch p.Segs[0] {
			case "self":
				if len(p.Segs) != 2 {
					return nil // through a relation: the other instance's may be unset
				}
			case "request", "actor", "result", otherInstance:
				return nil
			}
		}
	}
	body := x.Assert.String()
	if x.When != nil {
		body = "(" + x.When.String() + ") implies (" + body + ")"
	}
	body = selfPath.ReplaceAllString(body, "${1}"+otherInstance+".")
	e, err := ParseExpr("all(" + otherInstance + " in instances(" + subject.Name + "): " + body + ")")
	if err != nil {
		return nil
	}
	return &Rule{ID: x.ID + "@others", Kind: Invariant, Subject: x.Subject, Assert: e, Status: "active", Source: x.Source, Prose: x.Prose, StatementID: x.StatementID}
}

var selfPath = regexp.MustCompile(`(^|[^A-Za-z0-9_.])self\.`)

// quantifiesOver reports whether e ranges over a collection other than the
// instances of an entity that is not subject.
func quantifiesOver(e Expr, subject *Entity) bool {
	q := func(x Expr) bool { return quantifiesOver(x, subject) }
	switch e := e.(type) {
	case *Quant:
		p, ok := e.Coll.(*Path)
		if !ok || len(p.Segs) != 2 || p.Segs[1] != InstancesRelation || p.Segs[0] == lowerFirst(subject.Name) {
			return true
		}
		return q(e.Body)
	case *Binary:
		return q(e.L) || q(e.R)
	case *Not:
		return q(e.X)
	case *Call:
		return slices.ContainsFunc(e.Args, q)
	case *Scale:
		return q(e.X)
	case *Index:
		return q(e.X) || q(e.K)
	case *Set:
		return slices.ContainsFunc(e.Elems, q)
	case *ListLit:
		return slices.ContainsFunc(e.Elems, q)
	}
	return false
}
