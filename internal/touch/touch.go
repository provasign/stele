// Package touch finds the approved requirements a code change touches: the
// rules whose step or attributes name code the change adds, edits or
// removes. It says which rules the change touches, never whether the code
// keeps them; the coding agent reads them again before it finishes.
package touch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/provasign/stele/internal/codegraph"
	"github.com/provasign/stele/internal/ir"
)

// Touched is an approved statement a change touches, with why: each piece
// of evidence names a changed symbol and the term of a rule it matches.
type Touched struct {
	Statement string   `json:"statement"`
	Prose     string   `json:"prose"`
	Source    string   `json:"source,omitempty"`
	Rules     []string `json:"rules"`
	Approved  bool     `json:"approved"`
	Evidence  []string `json:"evidence"`
}

// Changes are the symbols a change touches in the repository: the working
// tree, untracked files included, against base.
func Changes(ctx context.Context, repo, base string) ([]codegraph.Symbol, error) {
	names, err := git(repo, "diff", "--name-only", "--no-renames", base, "--")
	if err != nil {
		return nil, err
	}
	untracked, err := git(repo, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(names+"\n"+untracked, "\n") {
		f = strings.TrimSpace(f)
		// Specs, evidence and Stele's own files are not the code.
		if f == "" || strings.HasPrefix(f, ".stele/") || strings.HasPrefix(f, ".shale/") || strings.HasPrefix(f, ".grove/") || slices.Contains(files, f) {
			continue
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, nil
	}
	g, err := codegraph.OpenParser(ctx, repo)
	if err != nil {
		return nil, err
	}
	defer g.Close()
	var out []codegraph.Symbol
	for _, f := range files {
		before, _ := exec.Command("git", "-C", repo, "show", base+":"+f).Output() // a new file has none
		after, _ := os.ReadFile(filepath.Join(repo, f))                           // a deleted file has none
		syms, err := g.Changed(f, before, after)
		if err != nil {
			continue // a file Grove cannot parse has no symbols to match
		}
		out = append(out, syms...)
	}
	return out, nil
}

// Rules returns the live statements of reg whose rules a changed symbol
// touches, in registry order:
//   - the symbol is the rule's step (a function or method named like the
//     operation, on its entity or on nothing),
//   - it is, reads or writes an attribute the rule reads, or
//   - it calls the rule's step.
//
// Names match ignoring case, separators and a get/set/is/has prefix, so
// booksOnLoan matches BooksOnLoan, books_on_loan and getBooksOnLoan.
func Rules(reg *ir.Registry, changed []codegraph.Symbol) []Touched {
	var out []Touched
	index := map[string]int{}
	for _, x := range reg.Rules {
		if !x.Live() || x.StatementID == "" {
			continue // only the owner's requirements, not Stele's world facts
		}
		ev := evidence(reg, x, changed)
		if len(ev) == 0 {
			continue
		}
		i, ok := index[x.StatementID]
		if !ok {
			i = len(out)
			index[x.StatementID] = i
			out = append(out, Touched{Statement: x.StatementID, Prose: x.Prose, Source: x.Source, Approved: true})
		}
		t := &out[i]
		t.Rules = append(t.Rules, x.ID)
		t.Approved = t.Approved && x.Approved()
		for _, e := range ev {
			if !slices.Contains(t.Evidence, e) {
				t.Evidence = append(t.Evidence, e)
			}
		}
	}
	return out
}

func evidence(reg *ir.Registry, x *ir.Rule, changed []codegraph.Symbol) []string {
	op := x.Operation
	if op == "" {
		op = x.On
	}
	entity, step, _ := strings.Cut(op, ".")
	if step == "" {
		entity, step = "", ""
	}
	subject, _ := reg.SubjectOf(x)
	var attrs []string
	for _, e := range []ir.Expr{x.When, x.Assert} {
		if e == nil {
			continue
		}
		for _, a := range reg.Reads(e, subject) {
			if !slices.Contains(attrs, a.Name) {
				attrs = append(attrs, a.Name)
			}
		}
	}
	var out []string
	for _, s := range changed {
		at := fmt.Sprintf("%s (%s:%d)", display(s), s.File, s.Line)
		if step != "" && same(s.Name, step) && (s.Parent == "" || same(last(s.Parent), entity)) && callable(s.Kind) {
			out = append(out, at+" is the step "+op)
			continue
		}
		why := ""
		for _, a := range attrs {
			switch {
			case same(s.Name, a) && !callable(s.Kind):
				why = " is " + a
			case same(s.Name, a):
				why = " computes " + a
			case slices.ContainsFunc(s.Attrs, func(n string) bool { return same(last(n), a) }) || slices.ContainsFunc(s.Calls, func(n string) bool { return same(last(n), a) }):
				why = " reads or writes " + a
			}
			if why != "" {
				break
			}
		}
		if why != "" {
			out = append(out, at+why)
			continue
		}
		if step != "" && slices.ContainsFunc(s.Calls, func(n string) bool { return same(last(n), step) }) {
			out = append(out, at+" calls the step "+op)
		}
	}
	return out
}

func callable(kind string) bool {
	return kind == "function" || kind == "method" || kind == "constructor"
}

func display(s codegraph.Symbol) string {
	if s.Qualified != "" {
		return s.Qualified
	}
	return s.Name
}

func last(name string) string {
	if i := strings.LastIndexAny(name, ".:#"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// same reports whether a code name and a vocabulary term name one thing.
func same(code, term string) bool {
	c, t := norm(code), norm(term)
	if c == "" || t == "" {
		return false
	}
	if c == t {
		return true
	}
	for _, p := range []string{"get", "set", "is", "has"} {
		if strings.TrimPrefix(c, p) == t || strings.TrimPrefix(t, p) == c {
			return true
		}
	}
	return false
}

func norm(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// Summary is the touched statements in a few lines, for a hook or a report;
// plain puts a statement's prose on one line.
func Summary(ts []Touched, k int, plain func(string) string) string {
	if len(ts) == 0 {
		return ""
	}
	sort.SliceStable(ts, func(i, j int) bool { return len(ts[i].Evidence) > len(ts[j].Evidence) })
	var b strings.Builder
	fmt.Fprintf(&b, "Stele: this code change touches %d approved requirement(s); check the change still keeps each:\n", len(ts))
	for i, t := range ts {
		if i == k {
			fmt.Fprintf(&b, "%d more; run `stele touches` for all.\n", len(ts)-k)
			break
		}
		prose := plain(t.Prose)
		where := ""
		if t.Source != "" {
			where = " (" + t.Source + ")"
		}
		mark := ""
		if !t.Approved {
			mark = " [not yet approved by the owner]"
		}
		fmt.Fprintf(&b, "- %s%s%s\n  because %s\n", prose, where, mark, strings.Join(t.Evidence[:min(2, len(t.Evidence))], "; "))
	}
	b.WriteString("Touching is not breaking: Stele matched names, it did not check the code against the rules.\n")
	return b.String()
}

func git(repo string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}
