package ir

import (
	"strings"
	"testing"
)

func TestParseExpr(t *testing.T) {
	cases := []struct{ src, want string }{
		{"self.status = destroyed", "(self.status = destroyed)"},
		{"not self.autoStoppedForIdle", "not self.autoStoppedForIdle"},
		{"a.x and b.y or c.z", "((a.x and b.y) or c.z)"},
		{"a.x implies b.y implies c.z", "(a.x implies (b.y implies c.z))"},
		{"(not a.x and b.n > 0) implies c.y", "((not a.x and (b.n > 0)) implies c.y)"},
		{"self.status in {stopped, destroyed}", "(self.status in {stopped, destroyed})"},
		{"self.a - 5 >= -3", "((self.a - 5) >= -3)"},
		{"self.a * 2 + self.b <= 10", "((self.a * 2 + self.b) <= 10)"},
		{"request.event = agentCompletedClean\n  and request.pushSucceeded", "((request.event = agentCompletedClean) and request.pushSucceeded)"},
		// D-050 types
		{`self.route = "/api/workspaces"`, `(self.route = "/api/workspaces")`},
		{`startsWith(self.route, "/api/") and not contains(self.cmd, "rm -rf")`, `(startsWith(self.route, "/api/") and not contains(self.cmd, "rm -rf"))`},
		{`self.tags contains "admin"`, `contains(self.tags, "admin")`},
		{"self.ratio <= 0.75 and self.fee > -1.5", "((self.ratio <= 0.75) and (self.fee > -1.5))"},
		{"self.idle >= 15m and self.grace < 500ms", "((self.idle >= 15m) and (self.grace < 500ms))"},
		{"self.createdAt < 2025-01-01", "(self.createdAt < 2025-01-01)"},
		{"self.at >= 2025-01-01T10:30:00Z", "(self.at >= 2025-01-01T10:30:00Z)"},
		{"self.limits[gold] > 3 and has(self.limits, silver)", "((self.limits[gold] > 3) and has(self.limits, silver))"},
		{"self.owner = none or isSet(self.owner)", "((self.owner = none) or isSet(self.owner))"},
		{"all(w in self.node.workspaces: w.status = running)", "all(w in self.node.workspaces: (w.status = running))"},
		{"count(x in self.tags: startsWith(x, \"a\")) <= 2", `(count(x in self.tags: startsWith(x, "a")) <= 2)`},
		{"exists(w in node.workspaces: w.busy)", "any(w in node.workspaces: w.busy)"},
		{"len(self.name) + 1 > 3", "((len(self.name) + 1) > 3)"},
	}
	for _, c := range cases {
		e, err := ParseExpr(c.src)
		if err != nil {
			t.Errorf("ParseExpr(%q): %v", c.src, err)
			continue
		}
		if got := e.String(); got != c.want {
			t.Errorf("ParseExpr(%q) = %s, want %s", c.src, got, c.want)
		}
	}
}

func TestParseExprErrors(t *testing.T) {
	cases := []struct{ src, want string }{
		{"count(self.holds where it.state = active) > 0", "count(x in collection: condition)"},
		{"self.createdAt < 2025-13-01", "bad number"},
		{"self.a * self.b > 0", "linear"},
		{`self.a = "unterminated`, "unterminated string"},
		{"startsWith(self.a)", "takes 2 argument"},
		{"all(w self.ws: w.x)", `expected "in"`},
		{"self.a = ", "end of expression"},
		{"self.a ! b", "use `not`"},
		{"(self.a = b", `expected ")"`},
	}
	for _, c := range cases {
		_, err := ParseExpr(c.src)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseExpr(%q) error = %v, want it to mention %q", c.src, err, c.want)
		}
	}
}
