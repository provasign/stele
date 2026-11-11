package ir

import "testing"

func TestRuleWords(t *testing.T) {
	parse := func(s string) Expr {
		t.Helper()
		e, err := ParseExpr(s)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	for _, c := range []struct {
		r    Rule
		want string
	}{
		{Rule{Kind: Permission, Effect: "deny", Operation: "Tool.lend", When: parse("count(t in instances(Tool): t.status = lent and t.borrowerEmail = request.email) >= 3")},
			"Refuse “lend” on a tool when the number of other tools where that tool's status is lent and that tool's borrower email is the request's email is at least 3"},
		{Rule{Kind: Permission, Effect: "deny", Operation: "Tool.lend", When: parse("isSet(self.reservedBy) and self.reservedBy != request.email")},
			"Refuse “lend” on a tool when the tool's reserved by is set and the tool's reserved by is not the request's email"},
		{Rule{Kind: Permission, Effect: "possible", Operation: "Tool.reserve", When: parse("self.status = available")},
			"“Reserve” on a tool must be able to run when the tool's status is available"},
		{Rule{Kind: Precondition, Operation: "Tool.lend", When: parse("not isSet(self.reservedBy) or self.reservedBy = request.email")},
			"“Lend” on a tool runs only when either the tool's reserved by is not set or the tool's reserved by is the request's email"},
		{Rule{Kind: Invariant, Subject: "Member", When: parse("self.status = ended"), Assert: parse("self.depositRefunded")},
			"Whenever the member's status is ended, the member's deposit refunded is true"},
		{Rule{Kind: Invariant, On: "Tool.reserve", Assert: parse("self.status in {reserved, lent}")},
			"After “reserve” on a tool, the tool's status is one of reserved, lent"},
		{Rule{Kind: Permission, Effect: "deny", Operation: "Tool.lend", When: parse("count(m in instances(Member): m.email = request.email and m.status = suspended) >= 1 and not request.actor.isSteward")},
			"Refuse “lend” on a tool when the number of members where that member's email is the request's email and that member's status is suspended is at least 1 and the request's actor is not steward"},
		{Rule{Kind: Invariant, Assert: parse("tool.overdue implies tool.status = lent")},
			"Always, if the tool's overdue is true then the tool's status is lent"},
	} {
		if got := c.r.Words(); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.r.Reading(), got, c.want)
		}
	}
}

// Every expression form ends: a function the words do not know is said as
// it is written, never by turning a value into a condition and back.
func TestRuleWordsEnds(t *testing.T) {
	for _, src := range []string{"self.holds = 0 and old(self.holds) >= 0", "any(t in instances(Tool): t.lost) = true", "len(self.items) > 2 * 3", "self.limits[gold] <= 4", "self.tags = []", "self.owner = none"} {
		e, err := ParseExpr(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if got := (&Rule{Kind: Invariant, Assert: e}).Words(); got == "" {
			t.Errorf("%s: empty", src)
		}
	}
	unknown := &Call{Fn: "f", Args: []Expr{&Path{Segs: []string{"self", "x"}}}}
	for _, e := range []Expr{unknown, &Not{X: unknown}, &Binary{Op: "=", L: unknown, R: &IntLit{V: 1}}} {
		if got := (&Rule{Kind: Invariant, Assert: e}).Words(); got == "" {
			t.Errorf("%s: empty", e)
		}
	}
}
