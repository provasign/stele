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
// invariants that read the instance's own attributes and quantify over
// nothing are restated: one about every other instance would compare an
// instance with itself. It is nil when the entity has no such relation.
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
		if quantifies(e) {
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

// quantifies reports whether e ranges over a collection.
func quantifies(e Expr) bool {
	switch e := e.(type) {
	case *Quant:
		return true
	case *Binary:
		return quantifies(e.L) || quantifies(e.R)
	case *Not:
		return quantifies(e.X)
	case *Call:
		return slices.ContainsFunc(e.Args, quantifies)
	case *Scale:
		return quantifies(e.X)
	case *Index:
		return quantifies(e.X) || quantifies(e.K)
	case *Set:
		return slices.ContainsFunc(e.Elems, quantifies)
	case *ListLit:
		return slices.ContainsFunc(e.Elems, quantifies)
	}
	return false
}
