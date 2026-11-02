package formalize

import "testing"

func TestRenameValue(t *testing.T) {
	cases := []struct{ in, from, to, want string }{
		{`self.outcome = error`, "error", "rejected", `self.outcome = rejected`},
		{`self.outcome in {error, loaded, error}`, "error", "rejected", `self.outcome in {rejected, loaded, rejected}`},
		{`contains(self.stderr, "error") and self.outcome = error`, "error", "rejected", `contains(self.stderr, "error") and self.outcome = rejected`},
		{`self.error.code = 1 and self.errors = none`, "error", "rejected", `self.error.code = 1 and self.errors = none`},
		{`self.status = failedLate`, "failed", "rejected", `self.status = failedLate`},
	}
	for _, c := range cases {
		if got := renameValue(c.in, c.from, c.to); got != c.want {
			t.Errorf("renameValue(%q, %q) = %q, want %q", c.in, c.from, got, c.want)
		}
	}
	if !mentions(`self.outcome != error`, "error") || mentions(`contains(self.err, "error")`, "error") {
		t.Error("mentions")
	}
}
