// Package bench measures translation on fixed benchmark sets. For each saved
// formalize run it reports how many statements ended checked (D-049), which
// known conflicts the checker finds, how close statements come to hand-written
// references when the run used the reference vocabulary, and how long the
// model calls took. It never calls a model: it scores runs already on disk.
package bench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/formalize"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/score"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
)

// Set is one benchmark set: frozen statements and what is known about them.
// Paths are relative to the sets file.
type Set struct {
	Name       string `yaml:"name"`
	Statements string `yaml:"statements"`
	Known      string `yaml:"known"`
	// Reference is a hand-written registry. Per-statement equivalence is only
	// meaningful for runs translated against its vocabulary.
	Reference string `yaml:"reference,omitempty"`
	// HeldOut sets are for measurement only: never inspected while changing
	// the translator (memory: any-project).
	HeldOut bool   `yaml:"heldOut,omitempty"`
	Note    string `yaml:"note,omitempty"`
}

// LoadSets reads a sets file and resolves its paths.
func LoadSets(path string) ([]Set, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sets []Set
	if err := yaml.Unmarshal(src, &sets); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dir := filepath.Dir(path)
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	seen := map[string]bool{}
	for i := range sets {
		s := &sets[i]
		if s.Name == "" || s.Statements == "" || seen[s.Name] {
			return nil, fmt.Errorf("%s: every set needs a unique name and a statements file (%q)", path, s.Name)
		}
		seen[s.Name] = true
		s.Statements, s.Known, s.Reference = abs(s.Statements), abs(s.Known), abs(s.Reference)
	}
	return sets, nil
}

// Timing summarizes the model calls journaled in requests.jsonl.
type Timing struct {
	Backends []string            `json:"backends"` // offline replays name themselves here
	Calls    formalize.CallStats `json:"calls"`
	// ActiveSeconds is the time at least one call was in flight, so pauses
	// between resumed sessions are not counted; CallSeconds sums every call.
	ActiveSeconds float64   `json:"active_seconds"`
	CallSeconds   float64   `json:"call_seconds"`
	LongestCall   float64   `json:"longest_call_seconds"`
	MaxInFlight   int       `json:"max_in_flight"`
	PromptBytes   int       `json:"prompt_bytes"`
	ReplyBytes    int       `json:"reply_bytes"`
	Usage         llm.Usage `json:"usage"`
}

// Report scores one run on one set.
type Report struct {
	Set        string         `json:"set"`
	HeldOut    bool           `json:"held_out,omitempty"`
	Run        string         `json:"run"`
	Statements int            `json:"statements"`
	States     map[string]int `json:"states,omitempty"` // formalize end states; empty without formalize.json
	// Checked counts translated statements and judgments reviewed as informal
	// (D-049); everything else is unchecked.
	Checked int `json:"checked"`
	// Missing lists the terms statements still lacked, most frequent first:
	// what the IR or vocabulary has to grow to cover.
	Missing   []Count             `json:"missing,omitempty"`
	LoadError string              `json:"load_error,omitempty"`
	Recall    *score.RecallResult `json:"recall,omitempty"`
	InCorpus  int                 `json:"known_in_corpus"`
	Reference *score.Result       `json:"reference,omitempty"`
	Timing    *Timing             `json:"timing,omitempty"`
}

// Count is a term and how many statements named it.
type Count struct {
	Term string `json:"term"`
	N    int    `json:"n"`
}

// Run scores the formalize output in dir against set.
func Run(ctx context.Context, z *solver.Z3, set Set, dir string) (*Report, error) {
	stmts, err := translate.LoadStatements(set.Statements)
	if err != nil {
		return nil, err
	}
	rep := &Report{Set: set.Name, HeldOut: set.HeldOut, Run: dir, Statements: len(stmts)}
	if err := rep.coverage(dir, stmts); err != nil {
		return nil, err
	}
	if rep.Timing, err = journal(filepath.Join(dir, "requests.jsonl")); err != nil {
		return nil, err
	}
	reg, err := ir.LoadDir(dir)
	var prog *lower.Program
	if err == nil {
		prog, err = lower.Lower(reg, lower.Options{IgnoreDecisions: true})
	}
	if err != nil {
		// A run that does not compile still has coverage and timing to report.
		rep.LoadError = err.Error()
		return rep, nil
	}
	if set.Known != "" {
		known, err := score.LoadKnown(set.Known)
		if err != nil {
			return nil, err
		}
		res, err := checker.Check(ctx, z, prog)
		if err != nil {
			return nil, err
		}
		rep.Recall = score.Recall("stele", known, score.CheckerFindings(res, stmts, reg), stmts)
		for _, k := range rep.Recall.Known {
			if !k.Unsegmented {
				rep.InCorpus++
			}
		}
	}
	if set.Reference != "" {
		if rep.Reference, err = score.Score(ctx, z, set.Reference, dir, stmts); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

// coverage reads the end state of every statement from formalize.json.
func (rep *Report) coverage(dir string, stmts []translate.Statement) error {
	src, err := os.ReadFile(filepath.Join(dir, "formalize.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var res formalize.Result
	if err := json.Unmarshal(src, &res); err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	want := map[string]bool{}
	for _, s := range stmts {
		want[s.ID] = true
	}
	rep.States = map[string]int{}
	terms := map[string]int{}
	for _, e := range res.Ends {
		if !want[e.ID] {
			continue
		}
		delete(want, e.ID)
		rep.States[e.State]++
		if e.State == "translated" || e.State == "informal" {
			rep.Checked++
		}
		for _, m := range e.Missing {
			terms[m]++
		}
	}
	// Statements the run never reached are unchecked too.
	if len(want) > 0 {
		rep.States["absent"] += len(want)
	}
	for t, n := range terms {
		rep.Missing = append(rep.Missing, Count{t, n})
	}
	sort.Slice(rep.Missing, func(i, j int) bool {
		if rep.Missing[i].N != rep.Missing[j].N {
			return rep.Missing[i].N > rep.Missing[j].N
		}
		return rep.Missing[i].Term < rep.Missing[j].Term
	})
	return nil
}

type event struct {
	ID           int       `json:"id"`
	Event        string    `json:"event"`
	At           time.Time `json:"at"`
	Backend      string    `json:"backend"`
	PromptBytes  int       `json:"prompt_bytes"`
	ReplyBytes   int       `json:"reply_bytes"`
	Milliseconds int64     `json:"elapsed_ms"`
	Usage        llm.Usage `json:"usage"`
}

// journal summarizes requests.jsonl; nil when the run has none.
func journal(path string) (*Timing, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	type span struct{ start, end time.Time }
	spans := map[int]*span{}
	t := &Timing{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		switch e.Event {
		case "started":
			if e.Backend != "" && !contains(t.Backends, e.Backend) {
				t.Backends = append(t.Backends, e.Backend)
			}
			t.Calls.Started++
			t.PromptBytes += e.PromptBytes
			spans[e.ID] = &span{start: e.At}
		case "completed", "failed":
			if e.Event == "completed" {
				t.Calls.Completed++
			} else {
				t.Calls.Failed++
			}
			t.ReplyBytes += e.ReplyBytes
			t.Usage.Add(e.Usage)
			secs := float64(e.Milliseconds) / 1000
			t.CallSeconds += secs
			if secs > t.LongestCall {
				t.LongestCall = secs
			}
			if s := spans[e.ID]; s != nil {
				s.end = e.At
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// Sweep call intervals for the time in flight and the peak concurrency.
	type edge struct {
		at    time.Time
		delta int
	}
	var edges []edge
	for _, s := range spans {
		if s.end.IsZero() {
			t.Calls.Interrupted++
			continue
		}
		edges = append(edges, edge{s.start, 1}, edge{s.end, -1})
	}
	sort.Slice(edges, func(i, j int) bool {
		if !edges[i].at.Equal(edges[j].at) {
			return edges[i].at.Before(edges[j].at)
		}
		return edges[i].delta < edges[j].delta // close before open at the same instant
	})
	open := 0
	var since time.Time
	for _, e := range edges {
		if open == 0 && e.delta > 0 {
			since = e.at
		}
		open += e.delta
		if open > t.MaxInFlight {
			t.MaxInFlight = open
		}
		if open == 0 && e.delta < 0 {
			t.ActiveSeconds += e.at.Sub(since).Seconds()
		}
	}
	return t, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
