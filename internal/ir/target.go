package ir

// Target is the tie by which an operation's rules name the instance it acts
// on: request.a = self.a, by the same name, a conjunct of the condition of some rule about the
// operation and its own entity, the same tie in every rule that has one. It
// is nil when no rule ties self to the request, or rules tie it differently.
func (r *Registry) Target(op string) *Binary {
	o := r.Operations[op]
	if o == nil || o.Entity == nil {
		return nil
	}
	var tie *Binary
	for _, x := range r.Rules {
		if !x.Live() || x.Op() != op || x.When == nil {
			continue
		}
		if s, err := r.SubjectOf(x); err != nil || s != o.Entity {
			continue
		}
		for _, c := range Conjuncts(x.When) {
			t := requestTie(c)
			if t == nil {
				continue
			}
			if tie != nil && t.String() != tie.String() {
				return nil
			}
			tie = t
		}
	}
	return tie
}

// ForTarget is x about operation op, read for the instance op acts on when x
// does not say which instance it is about: x's condition gains op's Target
// when x reads self and nothing of the request the tie reads. "Releasing a
// plot that is already free is rejected" was read for every free plot, and
// "a released plot with a holder goes to the first member waiting" for every
// held plot, so that releasing an unknown number clashed with it (a
// developer trial, 2026-11-05). It is x itself otherwise. tie is
// r.Target(op), passed in since it reads every rule.
func (r *Registry) ForTarget(op string, tie *Binary, x *Rule) *Rule {
	if tie == nil || x.Op() != op || x.Assume {
		return x
	}
	o := r.Operations[op]
	if s, err := r.SubjectOf(x); err != nil || o == nil || s != o.Entity {
		return x
	}
	req := tie.L.(*Path).String()
	self := false
	for _, e := range []Expr{x.When, x.Assert} {
		if e == nil {
			continue
		}
		for _, p := range Paths(e) {
			switch {
			case p.Segs[0] == "self":
				self = true
			case p.String() == req, p.Segs[0] == otherInstance:
				return x
			}
		}
	}
	if !self {
		return x
	}
	y := *x
	y.When = tie
	if x.When != nil {
		y.When = &Binary{Op: "and", L: x.When, R: tie}
	}
	return &y
}

// requestTie is c as request.a = self.a, or nil when it is not one.
func requestTie(c Expr) *Binary {
	b, ok := c.(*Binary)
	if !ok || b.Op != "=" {
		return nil
	}
	l, lok := b.L.(*Path)
	rt, rok := b.R.(*Path)
	if !lok || !rok || len(l.Segs) != 2 || len(rt.Segs) != 2 {
		return nil
	}
	if l.Segs[0] == "self" {
		l, rt = rt, l
	}
	if l.Segs[0] != "request" || rt.Segs[0] != "self" || l.Segs[1] != rt.Segs[1] {
		// A request field by another name is a condition, not the key the
		// request names the instance by: request.compression =
		// self.wholeFileCompression held a not-found rule to files stored
		// with the requested compression (a benchmark registry, 2026-11-06).
		return nil
	}
	return &Binary{Op: "=", L: l, R: rt}
}

// Conjuncts are e's top-level conjuncts.
func Conjuncts(e Expr) []Expr {
	if b, ok := e.(*Binary); ok && b.Op == "and" {
		return append(Conjuncts(b.L), Conjuncts(b.R)...)
	}
	return []Expr{e}
}
