package cli

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/provasign/stele/internal/card"
	"github.com/provasign/stele/internal/change"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/touch"
)

// writeCard records a change check on the change's card, for the PR: what
// it adds, edits and retires, and its status. The owner's decisions and
// approval already on the card are kept.
func writeCard(res *change.Result, head *ir.Registry, status, base string) error {
	c, err := card.Load(res.Repo, res.Change)
	if err != nil || c == nil {
		c = &card.Card{}
	}
	c.Change, c.Format, c.Base, c.Status = res.Change, res.Format, base, status
	if base, err := ir.LoadDir(res.Registry); err == nil {
		c.Checked = approvedStatements(base)
	}
	c.Requirements = nil
	add := func(id, what string) {
		r := card.Requirement{ID: id, Title: card.Title(res.Statements[id], id), Change: what, Source: res.Sources[id]}
		if what != "retired" && head != nil {
			for _, x := range head.Rules {
				if x.StatementID == id && x.Live() {
					r.Reading = append(r.Reading, change.Reading(x))
				}
			}
		}
		c.Requirements = append(c.Requirements, r)
	}
	for _, id := range res.Added {
		add(id, "added")
	}
	for _, id := range res.Modified {
		add(id, "modified")
	}
	for _, id := range res.Removed {
		add(id, "retired")
	}
	c.Open = 0
	for _, q := range res.Questions {
		if q.AskOwner && q.Kind != "approve" {
			c.Open++
		}
	}
	if status != "approved" {
		c.ApprovedBy, c.ApprovedAt = "", ""
	}
	refreshTouched(c, res.Repo, head)
	return card.Save(res.Repo, c)
}

// approvedStatements counts the statements with live approved rules.
func approvedStatements(reg *ir.Registry) int {
	var ids []string
	for _, x := range reg.Rules {
		if x.Live() && x.StatementID != "" && x.Approved() && !slices.Contains(ids, x.StatementID) {
			ids = append(ids, x.StatementID)
		}
	}
	return len(ids)
}

// recordDecision adds the owner's answer to the change's card, if it has one.
func recordDecision(repo, id string, q change.Question, label, by string) error {
	c, err := card.Load(repo, id)
	if err != nil || c == nil {
		return err
	}
	text := q.Text
	if q.Kind == "approve" {
		text = "Approve the change's rules?"
	}
	c.Decisions = append(c.Decisions, card.Decision{Question: text, Answer: label, By: by, At: time.Now().UTC().Format("2006-01-02")})
	if q.Kind == "approve" && label == "Approve" {
		c.Status, c.ApprovedBy, c.ApprovedAt, c.Open = "approved", by, time.Now().UTC().Format("2006-01-02"), 0
	}
	return card.Save(repo, c)
}

// refreshTouched puts on the card the approved requirements the branch's
// code touches now. A failure leaves the list as it was: the card says
// when it was written.
func refreshTouched(c *card.Card, repo string, reg *ir.Registry) {
	if reg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	changed, err := touch.Changes(ctx, repo, forkPoint(repo))
	if err != nil {
		return
	}
	c.Touched = nil
	for _, t := range touch.Rules(reg, changed) {
		if !t.Approved {
			continue // the change's own new rules are listed above
		}
		c.Touched = append(c.Touched, card.Touched{Statement: t.Statement, Title: card.Title(t.Prose, t.Statement), Evidence: t.Evidence[:min(3, len(t.Evidence))]})
	}
}

// refreshCards brings the cards this branch adds or edits up to date with
// its code before a commit, and stages them: the PR shows the requirements
// the committed code touches.
func refreshCards(root string, reg *ir.Registry) {
	base := forkPoint(root)
	out, err := exec.Command("git", "-C", root, "diff", "--name-only", base, "--", card.Dir).Output()
	if err != nil {
		return
	}
	untracked, _ := exec.Command("git", "-C", root, "ls-files", "--others", "--exclude-standard", "--", card.Dir).Output()
	for _, f := range strings.Fields(string(out) + " " + string(untracked)) {
		id := strings.TrimSuffix(strings.TrimPrefix(f, card.Dir+"/"), ".json")
		c, err := card.Load(root, id)
		if err != nil || c == nil {
			continue
		}
		before := slices.Clone(c.Touched)
		refreshTouched(c, root, reg)
		if slices.EqualFunc(before, c.Touched, func(a, b card.Touched) bool {
			return a.Statement == b.Statement && slices.Equal(a.Evidence, b.Evidence)
		}) {
			continue
		}
		if card.Save(root, c) == nil {
			_ = exec.Command("git", "-C", root, "add", "--", f).Run()
		}
	}
}
