// Package checker finds conflicts in a lowered registry: situations for which
// no response satisfies every active rule (docs/05#situation-and-response).
// It asks the question once per unit: once for the state, and once per
// operation for the step from any state satisfying the invariants.
//
// A conflict is reported as its minimal set of rules (the unsat core) and a
// counterexample situation. All minimal conflicting sets are enumerated, so no
// hand-made slices are needed: after each core is found, the next search must
// drop at least one of its rules.
package checker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

// Verdicts (docs/06-checker.md#output-contract).
const (
	Pass    = "pass"
	Fail    = "fail"
	Unknown = "unknown"
)

// MaxConflicts bounds new conflicts per unit so a badly broken registry still
// returns. An unfinished search is reported as undecided, never complete.
const MaxConflicts = 100

// Options controls conflict enumeration, without suppressing any findings.
type Options struct {
	MaxConflicts int // per unit; zero uses MaxConflicts
	// KnownConflicts are already recorded conflicts that do not consume the
	// limit. They are still checked and reported; only the gate's version-pinned
	// ratchet may tolerate them.
	KnownConflicts [][]string
	// Cache, when set, reuses the results of units whose questions are
	// unchanged since an earlier check: adding a spec re-checks only the
	// units its rules touch.
	Cache *Cache
}

// Cache holds per-unit results, keyed by a hash of everything the unit asks
// the solver. It is safe to keep across runs: a changed rule, vocabulary,
// solver version, limit or checker build changes the key.
//
// With Dir set, results also persist there, one file per key, so separate
// processes and branches share them. A key names the whole question, so a
// result found on another branch answers this branch's identical question.
type Cache struct {
	Units        map[string]UnitResult `json:"units"`
	Dir          string                `json:"-"`
	Hits, Misses int                   `json:"-"`

	mu sync.Mutex // steps are checked concurrently
}

// PersistDir is where DefaultCache keeps results; empty keeps none. The
// stele binary sets it; library callers and tests leave it empty.
var PersistDir string

// SharedCache, when set, is the cache DefaultCache returns: a long-running
// process keeps one in memory for every check it serves.
var SharedCache *Cache

// DefaultCache returns the shared cache, a cache persisted in PersistDir,
// or nil.
func DefaultCache() *Cache {
	if SharedCache != nil {
		return SharedCache
	}
	if PersistDir == "" {
		return nil
	}
	return &Cache{Dir: PersistDir}
}

// BuildID names this checker build (see buildID): a client and a server of
// different builds must not answer for each other.
func BuildID() string { return buildID() }

// lookup returns the cached result for key; a nil cache or empty key has none.
func (c *Cache) lookup(key string) (UnitResult, bool) {
	if c == nil || key == "" {
		return UnitResult{}, false
	}
	c.mu.Lock()
	r, ok := c.Units[key]
	c.mu.Unlock()
	if !ok && c.Dir != "" {
		// An unreadable or partial file is a miss: the unit is solved again.
		if b, err := os.ReadFile(filepath.Join(c.Dir, key+".json")); err == nil && json.Unmarshal(b, &r) == nil {
			ok = true
			c.remember(key, r)
		}
	}
	if ok {
		// Copies, so the reachability pass never edits what is cached.
		r.Conflicts = slices.Clone(r.Conflicts)
		for i := range r.Conflicts {
			if i < len(r.Before) {
				r.Conflicts[i].before = r.Before[i]
			}
		}
		r.Before = nil
	}
	c.mu.Lock()
	if ok {
		c.Hits++
	} else {
		c.Misses++
	}
	c.mu.Unlock()
	return r, ok
}

func (c *Cache) store(key string, r UnitResult) {
	if c == nil || key == "" {
		return
	}
	r.Before = nil
	for _, cf := range r.Conflicts {
		r.Before = append(r.Before, cf.before)
	}
	c.remember(key, r)
	if c.Dir == "" {
		return
	}
	// Written whole and renamed into place, so a reader never sees half.
	b, err := json.Marshal(r)
	if err != nil || os.MkdirAll(c.Dir, 0o755) != nil {
		return
	}
	tmp, err := os.CreateTemp(c.Dir, key+".*.tmp")
	if err != nil {
		return
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil && cerr == nil {
		err = os.Rename(tmp.Name(), filepath.Join(c.Dir, key+".json"))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
}

func (c *Cache) remember(key string, r UnitResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Units == nil {
		c.Units = map[string]UnitResult{}
	}
	c.Units[key] = r
}

// UnitResult is what checking one unit found.
type UnitResult struct {
	Conflicts []Conflict     `json:"conflicts,omitempty"`
	Dead      []DeadResponse `json:"dead,omitempty"`
	Vacuous   []Vacuous      `json:"vacuous,omitempty"`
	Undecided bool           `json:"undecided,omitempty"`
	NearLimit bool           `json:"near_limit,omitempty"`
	// Whole: this part of the state unit cannot be satisfied on its own,
	// so the state unit is checked whole (see sliced).
	Whole bool `json:"whole,omitempty"`
	// Unreachable are this unit's conflicts set aside as unreachable.
	Unreachable []Conflict `json:"unreachable,omitempty"`
	// Before holds, in the cache, each conflict's state before the step
	// (Conflict.before, by position): reachability is decided again from
	// it on every check.
	Before []map[string]string `json:"before,omitempty"`
}

// unitKey hashes the question a unit asks.
// cacheFormat changes when the checker asks a unit more than before, so
// cached results from an older checker are not reused (2: avoided rules;
// 3: refinement decides situations the quantified query left unknown;
// 4: conflicts are cached before reachability, with their states before).
const cacheFormat = "4"

func unitKey(u *lower.Unit, dts []string, version string, limit int, known map[string]bool) string {
	h := sha256.New()
	w := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	w(cacheFormat)
	w(buildID())
	w(version)
	w(fmt.Sprint(limit))
	w(u.Name)
	// Only the datatypes the unit's variables use: a new enum elsewhere in
	// the registry does not change this unit's question.
	for _, d := range dts {
		for _, v := range u.Vars {
			if v.Sort != "" && strings.Contains(d, "("+v.Sort+" ") {
				w(d)
				break
			}
		}
	}
	for _, v := range u.Vars {
		w(fmt.Sprintf("%s %s %v %s", v.Name, v.Sort, v.System, v.Range))
	}
	for _, group := range [][]*lower.Formula{u.Rules, u.Assumptions, u.Possible} {
		w("--")
		for _, f := range group {
			w(f.Rule.ID + " " + f.Term + " " + fmt.Sprint(f.Role) + " " + f.Guard + " " + f.When)
			for _, v := range f.Asserted {
				w(v.Name)
			}
		}
	}
	for _, t := range u.Fixed() {
		w(t)
	}
	keys := make([]string, 0, len(known))
	for k := range known {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w(k)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// buildID names the checker build, so a persisted result is never reused by
// a checker that might answer differently: the commit for a clean build,
// otherwise the executable's size and modification time.
var buildID = sync.OnceValue(func() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev, modified string
		for _, kv := range bi.Settings {
			switch kv.Key {
			case "vcs.revision":
				rev = kv.Value
			case "vcs.modified":
				modified = kv.Value
			}
		}
		if rev != "" && modified == "false" {
			return rev
		}
	}
	if exe, err := os.Executable(); err == nil {
		if fi, err := os.Stat(exe); err == nil {
			return fmt.Sprintf("%s %d %d", exe, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return ""
})

// Binding is one input value in a counterexample.
type Binding struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Conflict is one minimal set of rules that no response can satisfy together.
type Conflict struct {
	Unit           string    `json:"unit"` // "state", or the operation whose step has no valid outcome
	Rules          []string  `json:"rules"`
	Counterexample []Binding `json:"counterexample"` // the situation, limited to what the rules read
	Situation      []Binding `json:"situation"`      // the whole situation

	before map[string]string // raw solver values of the situation, by label
}

// Result is the outcome of checking one lowered program.
type Result struct {
	Check     string         `json:"check"`
	Verdict   string         `json:"verdict"`
	Conflicts []Conflict     `json:"conflicts"`
	Dead      []DeadResponse `json:"dead_responses,omitempty"`
	Vacuous   []Vacuous      `json:"vacuous,omitempty"`
	// Unreachable are step conflicts whose state before no step can
	// produce (ISSUES 3): reported, not failed on.
	Unreachable []Conflict `json:"unreachable,omitempty"`
	// Undecided lists the units the solver gave up on. The other units are
	// still checked; the verdict is unknown unless something failed.
	Undecided []string `json:"undecided,omitempty"`
	// NearLimit lists the units decided only by an answer that took at
	// least 80% of the solver timeout: on a slower machine they would be
	// undecided. Reported, not failed on.
	NearLimit []string `json:"near_limit,omitempty"`
	Solver    string   `json:"solver"`
}

// q is one unit being checked.
type q struct {
	z        *solver.Z3
	dts      []string
	u        *lower.Unit
	env, sys []*lower.Var
	limit    int
	known    map[string]bool
}

// Check enumerates every minimal conflict and dead response in prog.
func Check(ctx context.Context, z *solver.Z3, prog *lower.Program) (*Result, error) {
	return CheckWith(ctx, z, prog, Options{})
}

// CheckWith is Check with explicit enumeration limits and known-conflict hints.
func CheckWith(ctx context.Context, z *solver.Z3, prog *lower.Program, opt Options) (*Result, error) {
	if opt.MaxConflicts <= 0 {
		opt.MaxConflicts = MaxConflicts
	}
	known := map[string]bool{}
	for _, ids := range opt.KnownConflicts {
		known[conflictKey(ids)] = true
	}
	version, err := z.Version()
	if err != nil {
		return nil, err
	}
	res := &Result{Check: "transition-conflict", Verdict: Pass, Solver: version}
	// Assumptions (declared defaults, and rules that read nothing any
	// operation writes) must be able to hold together: if they cannot, no
	// situation exists, every other unit is vacuous, and requirements that
	// contradict each other would pass unseen.
	conflicting := map[string]bool{} // assumptions in an assumptions conflict
	if u := assumptionUnit(prog); u != nil {
		key := ""
		if opt.Cache != nil {
			// Cached like the other units (ISSUES 41).
			key = unitKey(u, prog.Datatypes, version, opt.MaxConflicts, known)
		}
		if r, ok := opt.Cache.lookup(key); ok {
			res.Conflicts = append(res.Conflicts, r.Conflicts...)
			if r.Undecided {
				res.Undecided = append(res.Undecided, AssumptionsUnit)
			}
			if r.NearLimit {
				res.NearLimit = append(res.NearLimit, AssumptionsUnit)
			}
			for _, cf := range r.Conflicts {
				for _, r := range cf.Rules {
					conflicting[r] = true
				}
			}
		} else {
			near := z.NearLimit()
			c := &q{z: z, dts: prog.Datatypes, u: u, limit: opt.MaxConflicts, known: known}
			asserted := map[string]bool{}
			for _, f := range u.Rules {
				for _, v := range f.Asserted {
					asserted[v.Name] = true
				}
			}
			for _, v := range u.Vars {
				if asserted[v.Name] {
					c.sys = append(c.sys, v)
				} else {
					c.env = append(c.env, v)
				}
			}
			unknown, err := c.conflicts(ctx, res)
			if err != nil {
				return nil, err
			}
			if unknown {
				res.Undecided = append(res.Undecided, AssumptionsUnit)
			}
			for _, cf := range res.Conflicts {
				for _, r := range cf.Rules {
					conflicting[r] = true
				}
			}
			nearLimit := z.NearLimit() > near
			if nearLimit {
				res.NearLimit = append(res.NearLimit, AssumptionsUnit)
			}
			opt.Cache.store(key, UnitResult{Conflicts: slices.Clone(res.Conflicts), Undecided: unknown, NearLimit: nearLimit})
		}
	}
	// Assumptions that conflict with each other are reported above; like a
	// compiler after an error, check the rest without them, or no situation
	// would exist and every other conflict would go unseen.
	asChecked := func(u *lower.Unit) *lower.Unit {
		if len(conflicting) == 0 {
			return u
		}
		cp := *u
		cp.Assumptions = nil
		for _, a := range u.Assumptions {
			if !conflicting[a.Rule.ID] {
				cp.Assumptions = append(cp.Assumptions, a)
			}
		}
		if u.Op != nil {
			// A state invariant already reported in a state conflict
			// would repeat it in every step: leave it out of the steps.
			cp.Rules = nil
			for _, f := range u.Rules {
				if !(conflicting[f.Rule.ID] && f.Rule.Kind == ir.Invariant && f.Rule.On == "") {
					cp.Rules = append(cp.Rules, f)
				}
			}
		}
		return &cp
	}
	results := make([]UnitResult, len(prog.Units))
	checkOne := func(i int) error {
		u := asChecked(prog.Units[i])
		ck := &checkRun{z: z, prog: prog, opt: opt, version: version, known: known, conflicting: conflicting}
		var err error
		if u.Op == nil && sliceState {
			results[i], err = ck.sliced(ctx, u)
		} else {
			results[i], err = ck.unit(ctx, u, false)
		}
		return err
	}
	// The state comes first, since the steps are checked without its
	// conflicts. The steps then only read what the state left, so they are
	// checked concurrently.
	var steps []int
	for i, u := range prog.Units {
		if u.Op != nil {
			steps = append(steps, i)
			continue
		}
		if err := checkOne(i); err != nil {
			return nil, err
		}
		if u.Name == "state" {
			// State invariants that conflict with each other are reported
			// here. The steps assume the state before them satisfies every
			// invariant, so with these the conflicting situations would not
			// exist there, and conflicts they hold would go unseen: check
			// the steps without them, as with conflicting assumptions.
			setAside(conflicting, results[i].Conflicts)
		}
	}
	if err := parallel(steps, checkOne); err != nil {
		return nil, err
	}
	for i, u := range prog.Units {
		r := results[i]
		res.Conflicts = append(res.Conflicts, r.Conflicts...)
		res.Dead = append(res.Dead, r.Dead...)
		res.Vacuous = append(res.Vacuous, r.Vacuous...)
		res.Unreachable = append(res.Unreachable, r.Unreachable...)
		if r.Undecided {
			res.Undecided = append(res.Undecided, u.Name)
		}
		if r.NearLimit {
			res.NearLimit = append(res.NearLimit, u.Name)
		}
	}
	switch {
	case len(res.Conflicts) > 0 || len(res.Dead) > 0:
		res.Verdict = Fail
	case len(res.Undecided) > 0:
		res.Verdict = Unknown
	}
	sort.SliceStable(res.Conflicts, func(i, j int) bool {
		return strings.Join(res.Conflicts[i].Rules, ",") < strings.Join(res.Conflicts[j].Rules, ",")
	})
	return res, nil
}

// parallel calls f for each index on up to half the CPUs (each unit runs up
// to three solvers at once) and returns the first error.
func parallel(idx []int, f func(int) error) error {
	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for range min(max(1, runtime.NumCPU()/2), len(idx)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := f(i); err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for _, i := range idx {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return first
}

// checkRun is what checking one unit needs from the whole check.
type checkRun struct {
	z           *solver.Z3
	prog        *lower.Program
	opt         Options
	version     string
	known       map[string]bool
	conflicting map[string]bool // assumptions and invariants set aside
}

// unit checks one unit, or answers it from the cache. A part of the state
// unit (see sliced) is first checked for being satisfiable on its own.
func (ck *checkRun) unit(ctx context.Context, u *lower.Unit, part bool) (UnitResult, error) {
	prog, opt, known, conflicting := ck.prog, ck.opt, ck.known, ck.conflicting
	// A solver of its own counts this unit's answers near the timeout.
	z := ck.z.Fork()
	key := ""
	if opt.Cache != nil {
		key = unitKey(u, prog.Datatypes, ck.version, opt.MaxConflicts, known)
		if part {
			key = "part-" + key
		}
		if r, ok := opt.Cache.lookup(key); ok {
			return ck.setAside(ctx, z, u, r)
		}
	}
	res := &Result{}
	c := &q{z: z, dts: prog.Datatypes, u: u, limit: opt.MaxConflicts, known: known}
	for _, v := range u.Vars {
		if v.System {
			c.sys = append(c.sys, v)
		} else {
			c.env = append(c.env, v)
		}
	}
	if part {
		st, err := c.satisfiable(ctx)
		if err != nil {
			return UnitResult{}, err
		}
		if st != solver.Sat {
			r := UnitResult{Whole: true}
			if ctx.Err() == nil {
				opt.Cache.store(key, r)
			}
			return r, nil
		}
	}
	vac, err := c.vacuous(ctx, conflicting)
	if err != nil {
		return UnitResult{}, err
	}
	res.Vacuous = append(res.Vacuous, vac...)
	unknown, err := c.conflicts(ctx, res)
	if err != nil {
		return UnitResult{}, err
	}
	// A rule that never applies because spec statements' invariants
	// rule out its situation (not because the world does) hides what it
	// conflicts with there: "warn before an idle stop" and "never warn"
	// together rule out a workspace idle long enough to auto-stop, which
	// hides the clash of auto-stopping with never auto-stopping. Check
	// the unit again without those invariants about the state before.
	if aside := excludedBy(u, vac); len(aside) > 0 && !unknown {
		cp := *u
		cp.Assumptions = nil
		for _, a := range u.Assumptions {
			if !aside[a.Rule.ID] {
				cp.Assumptions = append(cp.Assumptions, a)
			}
		}
		c2 := &q{z: z, dts: prog.Datatypes, u: &cp, limit: opt.MaxConflicts, known: known, sys: c.sys, env: c.env}
		more := &Result{}
		u2, err := c2.conflicts(ctx, more)
		if err != nil {
			return UnitResult{}, err
		}
		unknown = unknown || u2
		for _, cf := range more.Conflicts {
			if !hasConflict(res.Conflicts, cf.Rules) {
				res.Conflicts = append(res.Conflicts, cf)
			}
		}
	}
	if !unknown {
		dead, u2, err := c.deadResponses(ctx)
		if err != nil {
			return UnitResult{}, err
		}
		res.Dead = append(res.Dead, dead...)
		unknown = u2
	}
	if !unknown {
		cs, u3, err := c.impossible(ctx)
		if err != nil {
			return UnitResult{}, err
		}
		res.Conflicts = append(res.Conflicts, cs...)
		unknown = u3
	}
	r := UnitResult{
		Conflicts: res.Conflicts,
		Dead:      res.Dead,
		Vacuous:   res.Vacuous,
		Undecided: unknown,
		NearLimit: z.NearLimit() > 0,
	}
	// A cancelled or timed-out check answers unknown for want of time, not
	// because the solver could not decide: stored, it was reused by every
	// later check without trying again (a code review, 2026-11-03).
	if ctx.Err() == nil {
		opt.Cache.store(key, r)
	}
	return ck.setAside(ctx, z, u, r)
}

// setAside moves the step conflicts from a state no trace enters out of
// r.Conflicts. Whether a state is reachable depends on every operation, not
// only the unit's, so it is decided on every check, cached result or not: a
// cached step kept passing after another step was changed to enter the
// state it conflicts in (a code review, 2026-11-03).
func (ck *checkRun) setAside(ctx context.Context, z *solver.Z3, u *lower.Unit, r UnitResult) (UnitResult, error) {
	if u.Op == nil || r.Whole {
		return r, nil
	}
	var kept, gone []Conflict
	for _, cf := range r.Conflicts {
		off, err := unreachable(ctx, z, ck.prog, cf)
		if err != nil {
			return UnitResult{}, err
		}
		if off {
			gone = append(gone, cf)
		} else {
			kept = append(kept, cf)
		}
	}
	r.Conflicts, r.Unreachable = kept, append(slices.Clip(r.Unreachable), gone...)
	return r, nil
}

func (c *q) conflicts(ctx context.Context, res *Result) (bool, error) {
	var found [][]int
	var described []Conflict
	unknown := false
	newConflicts := 0
	for len(c.u.Rules) > 0 {
		status, witness, selected, err := c.findSituation(ctx, found)
		if err != nil {
			return false, err
		}
		if status == solver.Unknown {
			unknown = true
			break
		}
		if status == solver.Unsat {
			break
		}
		core, err := c.minimalCore(ctx, witness, selected)
		if err != nil {
			return false, err
		}
		if core == nil {
			unknown = true
			break
		}
		conflict := c.describe(core, witness)
		if !c.known[conflictKey(conflict.Rules)] {
			if newConflicts >= c.limit {
				// A further core exists. Reporting the prefix as complete could
				// let a ratchet tolerate it all and admit an unchecked conflict.
				unknown = true
				break
			}
			newConflicts++
		}
		found = append(found, core)
		described = append(described, conflict)
	}
	// Each core is minimal in the situation it was found in. One found early
	// can still contain a core found later, in another situation; only the
	// smaller is a minimal conflict set. Every minimal set is found, because
	// the search ends only when every remaining selection contains a found core.
	for i, a := range found {
		if !containsAnother(a, i, found) {
			res.Conflicts = append(res.Conflicts, described[i])
		}
	}
	return unknown, nil
}

func conflictKey(ids []string) string {
	ids = append([]string{}, ids...)
	sort.Strings(ids)
	return strings.Join(ids, "\x00")
}

// containsAnother reports whether core i strictly contains another found core.
func containsAnother(a []int, i int, found [][]int) bool {
	in := map[int]bool{}
	for _, x := range a {
		in[x] = true
	}
	for j, b := range found {
		if j == i || len(b) >= len(a) {
			continue
		}
		sub := true
		for _, x := range b {
			sub = sub && in[x]
		}
		if sub {
			return true
		}
	}
	return false
}

func assumptionID(f *lower.Formula) string {
	if f.Rule == nil {
		return ""
	}
	return f.Rule.ID
}

func selName(i int) string { return fmt.Sprintf("|sel:%d|", i) }

// impossible checks each permission with effect possible: some situation
// and response must satisfy its guard, the operation running and every rule
// of the unit. When none does, the rules that rule it out (a minimal set)
// conflict with it; the conflict has no situation, since it is about all of
// them.
func (c *q) impossible(ctx context.Context) ([]Conflict, bool, error) {
	var out []Conflict
	for _, p := range c.u.Possible {
		asm := make([]bool, len(c.u.Assumptions)) // assumptions left out
		query := func(set []int) (solver.Status, error) {
			var b strings.Builder
			c.preamble(&b)
			declare(&b, c.env)
			declare(&b, c.sys)
			for i, a := range c.u.Assumptions {
				if !asm[i] {
					fmt.Fprintf(&b, "(assert %s) ; %s\n", a.Term, a.Rule.ID)
				}
			}
			for _, t := range c.u.Fixed() {
				fmt.Fprintf(&b, "(assert %s)\n", t)
			}
			fmt.Fprintf(&b, "(assert %s)\n", p.Term)
			for _, i := range set {
				fmt.Fprintf(&b, "(assert %s)\n", c.u.Rules[i].Term)
			}
			b.WriteString("(check-sat)\n")
			r, err := c.z.Run(ctx, b.String())
			if err != nil {
				return "", err
			}
			return r.Status, nil
		}
		all := make([]int, len(c.u.Rules))
		for i := range all {
			all[i] = i
		}
		st, err := query(all)
		if err != nil {
			return nil, false, err
		}
		switch st {
		case solver.Sat:
			continue
		case solver.Unknown:
			return out, true, nil
		}
		core := all
		if shrunk, err := c.solverCore(ctx, func(b *strings.Builder) { fmt.Fprintf(b, "(assert %s)\n", p.Term) }, all); err != nil {
			return nil, false, err
		} else if shrunk != nil {
			core = shrunk
		}
		for k := 0; k < len(core); {
			trial := append(append([]int{}, core[:k]...), core[k+1:]...)
			st, err := query(trial)
			if err != nil {
				return nil, false, err
			}
			switch st {
			case solver.Unsat:
				core = trial
			case solver.Sat:
				k++
			default:
				return out, true, nil
			}
		}
		ids := []string{p.Rule.ID}
		for _, i := range core {
			if !slices.Contains(ids, c.u.Rules[i].Rule.ID) {
				ids = append(ids, c.u.Rules[i].Rule.ID)
			}
		}
		// What holds before the step can rule the situation out too ("a
		// member never holds more than 5" against "possible with 9"): the
		// invariants it needs are part of the conflict, or a clash between
		// two requirements reads as one contradicting itself.
		// In rule order, so the set does not depend on how the unit was built.
		order := make([]int, len(c.u.Assumptions))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(x, y int) bool {
			return assumptionID(c.u.Assumptions[order[x]]) < assumptionID(c.u.Assumptions[order[y]])
		})
		for _, i := range order {
			a := c.u.Assumptions[i]
			asm[i] = true
			st, err := query(core)
			if err != nil {
				return nil, false, err
			}
			switch st {
			case solver.Unsat:
				continue // not needed
			case solver.Sat:
				asm[i] = false
				if a.Rule != nil && !slices.Contains(ids, a.Rule.ID) {
					ids = append(ids, a.Rule.ID)
				}
			default:
				return out, true, nil
			}
		}
		sort.Strings(ids)
		out = append(out, Conflict{Unit: c.u.Name, Rules: ids})
	}
	return out, false, nil
}

func (c *q) preamble(b *strings.Builder) {
	b.WriteString("(set-logic ALL)\n")
	for _, d := range c.dts {
		b.WriteString(d + "\n")
	}
}

func declare(b *strings.Builder, vs []*lower.Var) {
	for _, v := range vs {
		fmt.Fprintf(b, "(declare-const %s %s)\n", v.Name, v.Sort)
		if v.Range != "" {
			fmt.Fprintf(b, "(assert %s)\n", v.Range)
		}
	}
}

func (c *q) assumptions(b *strings.Builder) {
	for _, a := range c.u.Assumptions {
		fmt.Fprintf(b, "(assert %s) ; %s\n", a.Term, a.Rule.ID)
	}
}

// findSituation asks for a situation and a set of selected rules, not
// containing any core already found, such that no response satisfies the
// selected rules together with the unit's fixed constraints.
//
// "No response" quantifies over the response. Three ways of deciding it
// race, and the first definite answer wins: refinement over concrete
// responses, whose queries have no quantifier; the quantified query under
// quantifier elimination; and the quantified query under z3's default
// engine. Each answer is exact, and each way is fast where the others can
// take the whole timeout (HANDOFF-performance, 2026-11-01). The search is
// unknown only when all three are.
func (c *q) findSituation(ctx context.Context, found [][]int) (solver.Status, map[string]string, []int, error) {
	if len(c.sys) == 0 {
		return c.quantified(ctx, found, plainCheck)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type answer struct {
		st       solver.Status
		witness  map[string]string
		selected []int
		err      error
	}
	ways := []func() answer{
		func() answer {
			st, w, sel, err := c.quantified(ctx, found, qeCheck)
			return answer{st, w, sel, err}
		},
		func() answer {
			st, w, sel, err := c.quantified(ctx, found, plainCheck)
			return answer{st, w, sel, err}
		},
	}
	if refineSituations {
		ways = append(ways, func() answer {
			st, w, sel, ok := c.refine(ctx, found)
			if !ok {
				st = solver.Unknown
			}
			return answer{st, w, sel, nil}
		})
	}
	ch := make(chan answer, len(ways))
	for _, way := range ways {
		go func() { ch <- way() }()
	}
	var err error
	for range ways {
		a := <-ch
		if a.err == nil && a.st != solver.Unknown {
			return a.st, a.witness, a.selected, nil
		}
		if err == nil {
			err = a.err
		}
	}
	if err != nil {
		return "", nil, nil, err
	}
	return solver.Unknown, nil, nil, nil
}

const (
	plainCheck = "(check-sat)\n"
	// Z3's default quantifier engine gives up on many of these scripts over
	// integers; quantifier elimination decides linear integer arithmetic
	// completely, if slowly.
	qeCheck = "(check-sat-using (then qe smt))\n"
)

// refineSituations is off only in tests that compare the quantified query
// with refinement.
var refineSituations = true

// maxRefinements bounds the responses refine collects before it gives up and
// leaves the question to the quantified query. Over finite responses
// refinement always ends; over integers it may not.
const maxRefinements = 32

// refine decides findSituation's question by counterexample-guided
// refinement. It asks for a situation and selection that every response
// collected so far fails, then for a response that satisfies the selection
// there. None found: the situation is a conflict. One found: it joins the
// collected responses and the search repeats. No situation left: every
// situation has one of the collected responses, so none conflicts. Both
// answers are exact; ok is false when the solver did not settle the question
// within maxRefinements.
func (c *q) refine(ctx context.Context, found [][]int) (solver.Status, map[string]string, []int, bool) {
	var responses []string // let bindings of the response variables
	terms := c.responseTerms()
	// Whether each response variable equals each of its terms, in the order
	// the response query asks: equality, not printed values, since two
	// equal maps can print differently.
	type pair struct{ v, t string }
	var asked []pair
	for _, v := range c.sys {
		for _, t := range terms[v.Name] {
			asked = append(asked, pair{v.Name, t})
		}
	}
	// What a response must meet: every range and every selected rule.
	var ranges []string
	for _, v := range c.sys {
		if v.Range != "" {
			ranges = append(ranges, v.Range)
		}
	}
	holds := c.selectedBody()
	if len(ranges) > 0 {
		holds = "(and " + strings.Join(ranges, " ") + " " + holds + ")"
	}
	for range maxRefinements {
		var b strings.Builder
		b.WriteString("(set-option :produce-models true)\n")
		c.preamble(&b)
		declare(&b, c.env)
		c.assumptions(&b)
		for i := range c.u.Rules {
			fmt.Fprintf(&b, "(declare-const %s Bool)\n", selName(i))
		}
		for _, core := range found {
			sels := make([]string, len(core))
			for k, i := range core {
				sels[k] = selName(i)
			}
			fmt.Fprintf(&b, "(assert (not (and %s)))\n", strings.Join(sels, " "))
		}
		for _, r := range responses {
			fmt.Fprintf(&b, "(assert (not (let (%s) %s)))\n", r, holds)
		}
		b.WriteString("(check-sat)\n")
		names := make([]string, 0, len(c.env)+len(c.u.Rules))
		for _, v := range c.env {
			names = append(names, v.Name)
		}
		for i := range c.u.Rules {
			names = append(names, selName(i))
		}
		fmt.Fprintf(&b, "(get-value (%s))\n", strings.Join(names, " "))
		r, err := c.z.Run(ctx, b.String())
		if err != nil || r.Status == solver.Unknown {
			return "", nil, nil, false
		}
		if r.Status == solver.Unsat {
			return solver.Unsat, nil, nil, true
		}
		values := replyValues(r)
		if !hasAll(values, names) {
			return "", nil, nil, false
		}
		witness := map[string]string{}
		for _, v := range c.env {
			if val, ok := values[v.Name]; ok {
				witness[v.Name] = val
			}
		}
		var selected []int
		for i := range c.u.Rules {
			if values[selName(i)] == "true" {
				selected = append(selected, i)
			}
		}
		// A response satisfying the selected rules in that situation.
		b.Reset()
		b.WriteString("(set-option :produce-models true)\n")
		c.preamble(&b)
		declare(&b, c.env)
		declare(&b, c.sys)
		c.assumptions(&b)
		c.fixWitness(&b, witness)
		for _, t := range c.u.Fixed() {
			fmt.Fprintf(&b, "(assert %s)\n", t)
		}
		for _, i := range selected {
			fmt.Fprintf(&b, "(assert %s)\n", c.u.Rules[i].Term)
		}
		// The most general response: one that also meets the rules not
		// selected answers every selection here, and one that keeps values
		// answers more situations. Soft constraints only rank responses;
		// whichever is found meets the selection.
		for i, f := range c.u.Rules {
			if !slices.Contains(selected, i) {
				fmt.Fprintf(&b, "(assert-soft %s :weight 4)\n", f.Term)
			}
		}
		// Keeping the value, or the first term a rule names, is preferred;
		// any term answers more situations than a constant does.
		for _, v := range c.sys {
			for k, t := range terms[v.Name] {
				fmt.Fprintf(&b, "(assert-soft (= %s %s) :weight %d)\n", v.Name, t, max(1, 2-k))
			}
		}
		b.WriteString("(check-sat)\n")
		names = names[:0]
		for _, v := range c.sys {
			names = append(names, v.Name)
		}
		fmt.Fprintf(&b, "(get-value (%s))\n", strings.Join(names, " "))
		for _, p := range asked {
			fmt.Fprintf(&b, "(get-value ((= %s %s)))\n", p.v, p.t)
		}
		r, err = c.z.Run(ctx, b.String())
		if err != nil || r.Status == solver.Unknown {
			return "", nil, nil, false
		}
		if r.Status == solver.Unsat {
			return solver.Sat, witness, selected, true
		}
		values = replyValues(r)
		equals := map[pair]bool{}
		for k, p := range asked {
			if k+1 < len(r.Replies) {
				if l := r.Replies[k+1].List; len(l) == 1 && len(l[0].List) == 2 {
					equals[p] = l[0].List[1].String() == "true"
				}
			}
		}
		var binds []string
		for _, v := range c.sys {
			val, ok := values[v.Name]
			if !ok {
				return "", nil, nil, false
			}
			// A value that equals a term over the situation is collected as
			// the term: "unchanged", or "named after the workspace", then
			// answers every situation, not only those with this value.
			for _, t := range terms[v.Name] {
				if equals[pair{v.Name, t}] {
					val = t
					break
				}
			}
			binds = append(binds, fmt.Sprintf("(%s %s)", v.Name, val))
		}
		responses = append(responses, strings.Join(binds, " "))
	}
	return "", nil, nil, false
}

// responseTerms gives, for each response variable, the terms over the
// situation it may equal: its value before the step, any term a rule or the
// frame equates it with, "(= v' t)", and the terms candidates.go derives
// from bounds and contained strings. A term that names another response
// variable is resolved through that variable's own terms. Refinement
// collects a response as these terms where it can.
func (c *q) responseTerms() map[string][]string {
	sys := map[string]bool{}
	sorts := map[string]string{}
	for _, v := range c.env {
		sorts[v.Name] = v.Sort
	}
	for _, v := range c.sys {
		sys[v.Name], sorts[v.Name] = true, v.Sort
	}
	out := map[string][]string{}
	add := func(v, t string) {
		// A constant answers no more situations than a constant response,
		// and preferring one pulled a list's length to its lower bound.
		if es, err := solver.Parse(t); err != nil || len(es) != 1 || constant(es[0]) {
			return
		}
		if !slices.Contains(out[v], t) && len(out[v]) < maxTermsPerVar {
			out[v] = append(out[v], t)
		}
	}
	byLabel := map[string]*lower.Var{}
	for _, v := range c.env {
		byLabel[v.Label] = v
	}
	for _, v := range c.sys {
		if pre := byLabel[strings.TrimSuffix(v.Label, "'")]; v.Post && pre != nil && pre.Sort == v.Sort {
			add(v.Name, pre.Name)
		}
	}
	var mentions func(e solver.SExpr) bool
	mentions = func(e solver.SExpr) bool {
		if !e.IsList() {
			return !e.Str && sys[e.Atom]
		}
		return slices.ContainsFunc(e.List, mentions)
	}
	var equal, other []rawTerm
	// guard is the condition, over the situation alone, under which e is
	// read: the antecedents of the implications around it.
	guard := func(gs []solver.SExpr) *solver.SExpr {
		var keep []string
		for _, g := range gs {
			if !mentions(g) && !strings.Contains(g.String(), "|sel:") {
				keep = append(keep, g.String())
			}
		}
		if len(keep) == 0 {
			return nil
		}
		es, err := solver.Parse(conjoin(keep))
		if err != nil || len(es) != 1 {
			return nil
		}
		return &es[0]
	}
	var walk func(e solver.SExpr, gs []solver.SExpr)
	walk = func(e solver.SExpr, gs []solver.SExpr) {
		if !e.IsList() || len(e.List) == 0 {
			return
		}
		isSys := func(a solver.SExpr) bool { return !a.IsList() && !a.Str && sys[a.Atom] }
		switch e.List[0].Atom {
		case "forall", "exists", "let", "lambda":
			return // a term there may name a bound variable
		case "=":
			if len(e.List) == 3 {
				for k := 1; k <= 2; k++ {
					a, t := e.List[k], e.List[3-k]
					if isSys(a) {
						equal = append(equal, rawTerm{v: a.Atom, t: t})
					}
					if m, key, ok := mapEntry(a, sys); ok {
						other = append(other, rawTerm{v: m, t: t, key: &key, when: guard(gs)})
					}
				}
			}
		case "str.contains":
			if len(e.List) == 3 && isSys(e.List[1]) {
				other = append(other, rawTerm{v: e.List[1].Atom, t: e.List[2], contains: true})
			}
		case "str.prefixof", "str.suffixof":
			if len(e.List) == 3 && isSys(e.List[2]) {
				other = append(other, rawTerm{v: e.List[2].Atom, t: e.List[1], contains: true})
			}
		case "<=", "<", ">=", ">":
			for _, r := range boundTerms(e, sorts, sys) {
				if r.key != nil {
					r.when = guard(gs)
				}
				other = append(other, r)
			}
		case "=>":
			if len(e.List) == 3 {
				walk(e.List[1], gs)
				walk(e.List[2], append(slices.Clip(gs), e.List[1]))
				return
			}
		}
		for _, x := range e.List {
			walk(x, gs)
		}
	}
	for _, t := range c.u.Fixed() {
		if es, err := solver.Parse(t); err == nil {
			for _, e := range es {
				walk(e, nil)
			}
		}
	}
	for _, f := range c.u.Rules {
		if es, err := solver.Parse(f.Term); err == nil {
			for _, e := range es {
				walk(e, nil)
			}
		}
	}
	// Equalities free of response variables first, in the order found:
	// they were the only terms before, and the first is preferred.
	for _, r := range equal {
		if !mentions(r.t) {
			add(r.v, r.t.String())
		}
	}
	// resolve replaces each response variable t names by that variable's
	// terms, one at a time, keeping the results that name none.
	var resolve func(t solver.SExpr, depth int) []string
	resolve = func(t solver.SExpr, depth int) []string {
		if !mentions(t) {
			return []string{t.String()}
		}
		if depth == 0 {
			return nil
		}
		var w string
		var find func(e solver.SExpr)
		find = func(e solver.SExpr) {
			if w != "" {
				return
			}
			if !e.IsList() {
				if !e.Str && sys[e.Atom] {
					w = e.Atom
				}
				return
			}
			for _, x := range e.List {
				find(x)
			}
		}
		find(t)
		var got []string
		for _, s := range out[w] {
			r, err := solver.Parse(s)
			if err != nil || len(r) != 1 {
				continue
			}
			got = append(got, resolve(substitute(t, w, r[0]), depth-1)...)
			if len(got) >= maxTermsPerVar {
				break
			}
		}
		return got
	}
	contained := map[string][]string{}
	note := func(v, t string) {
		if !slices.Contains(contained[v], t) {
			contained[v] = append(contained[v], t)
		}
	}
	type entry struct{ key, value, when string }
	entries := map[string][]entry{}
	for range 3 { // a term may resolve through one found in an earlier round
		for _, r := range slices.Concat(equal, other) {
			if r.key != nil {
				keys := resolve(*r.key, 3)
				for _, t := range resolve(r.t, 3) {
					for _, k := range keys {
						when := ""
						if r.when != nil {
							when = r.when.String()
						}
						if e := (entry{k, t, when}); !slices.Contains(entries[r.v], e) && len(entries[r.v]) < maxTermsPerVar {
							entries[r.v] = append(entries[r.v], e)
						}
					}
				}
				continue
			}
			for _, t := range resolve(r.t, 3) {
				add(r.v, t)
				if r.contains {
					note(r.v, t)
				}
			}
		}
	}
	// The positions of a list prefer different terms, position i the i-th:
	// "lists every tool" is then answered by one response placing each
	// tool, where every position preferring the first term answered only
	// the situations whose tools coincide.
	for _, v := range c.sys {
		m := listPosition.FindStringSubmatch(v.Label)
		if m == nil {
			continue
		}
		i, _ := strconv.Atoi(m[1])
		ts := out[v.Name]
		if v.Post && len(ts) > 0 {
			ts = ts[1:] // keeping the value stays first
		}
		if len(ts) > 1 {
			k := i % len(ts)
			rotated := slices.Concat(ts[k:], ts[:k])
			copy(ts, rotated)
		}
	}
	// A map is its entries, stored over a constant map of the first value:
	// one term answers every situation the entries name.
	for m, es := range entries {
		t := fmt.Sprintf("((as const %s) %s)", sorts[m], es[0].value)
		for _, e := range es {
			if e.when == "" {
				t = fmt.Sprintf("(store %s %s %s)", t, e.key, e.value)
			} else {
				// Only where the rule gives the entry: a slot that holds
				// no item may share its key with one that does.
				t = fmt.Sprintf("(let ((m %s)) (ite %s (store m %s %s) m))", t, e.when, e.key, e.value)
			}
		}
		add(m, t)
	}
	// A value that must contain several strings may be all of them joined.
	for v, ts := range contained {
		if len(ts) > 1 {
			add(v, "(str.++ "+strings.Join(ts[:min(len(ts), 6)], " ")+")")
		}
	}
	return out
}

// conjoin is the conjunction of terms.
func conjoin(ts []string) string {
	if len(ts) == 1 {
		return ts[0]
	}
	return "(and " + strings.Join(ts, " ") + ")"
}

// constant reports whether e names no symbol: a literal, or operators over
// literals.
func constant(e solver.SExpr) bool {
	if !e.IsList() {
		if e.Str {
			return true
		}
		_, err := strconv.ParseFloat(e.Atom, 64)
		return err == nil || e.Atom == "true" || e.Atom == "false"
	}
	for _, x := range e.List[1:] {
		if !constant(x) {
			return false
		}
	}
	return len(e.List) > 0 && !e.List[0].IsList()
}

// listPosition matches the label of a list's element: Result.ids#2.
var listPosition = regexp.MustCompile(`#(\d+)'?$`)

// selectedBody is the conjunction the response must satisfy: the unit's
// fixed constraints and each rule its selector selects.
func (c *q) selectedBody() string {
	body := c.u.Fixed()
	for i, f := range c.u.Rules {
		body = append(body, fmt.Sprintf("(=> %s %s)", selName(i), f.Term))
	}
	return "(and " + strings.Join(body, " ") + ")"
}

// hasAll reports whether a get-value reply gave every name a value.
func hasAll(values map[string]string, names []string) bool {
	for _, n := range names {
		if _, ok := values[n]; !ok {
			return false
		}
	}
	return true
}

// replyValues reads a get-value reply into values by name.
func replyValues(r *solver.Result) map[string]string {
	values := map[string]string{}
	if len(r.Replies) > 0 {
		for _, pair := range r.Replies[0].List {
			if len(pair.List) == 2 {
				values[pair.List[0].Atom] = pair.List[1].String()
			}
		}
	}
	return values
}

// quantified asks findSituation's question as one query with the response
// quantified, decided by check.
func (c *q) quantified(ctx context.Context, found [][]int, check string) (solver.Status, map[string]string, []int, error) {
	var b strings.Builder
	b.WriteString("(set-option :produce-models true)\n")
	c.preamble(&b)
	declare(&b, c.env)
	c.assumptions(&b)
	for i := range c.u.Rules {
		fmt.Fprintf(&b, "(declare-const %s Bool)\n", selName(i))
	}
	for _, core := range found {
		sels := make([]string, len(core))
		for k, i := range core {
			sels[k] = selName(i)
		}
		fmt.Fprintf(&b, "(assert (not (and %s)))\n", strings.Join(sels, " "))
	}
	conj := c.selectedBody()
	if len(c.sys) == 0 {
		fmt.Fprintf(&b, "(assert (not %s))\n", conj)
	} else {
		var binders, ranges []string
		for _, v := range c.sys {
			binders = append(binders, fmt.Sprintf("(%s %s)", v.Name, v.Sort))
			if v.Range != "" {
				ranges = append(ranges, v.Range)
			}
		}
		guard := "true"
		if len(ranges) > 0 {
			guard = "(and " + strings.Join(ranges, " ") + ")"
		}
		fmt.Fprintf(&b, "(assert (forall (%s) (=> %s (not %s))))\n", strings.Join(binders, " "), guard, conj)
	}
	b.WriteString(check)
	names := make([]string, 0, len(c.env)+len(c.u.Rules))
	for _, v := range c.env {
		names = append(names, v.Name)
	}
	for i := range c.u.Rules {
		names = append(names, selName(i))
	}
	if len(names) > 0 {
		fmt.Fprintf(&b, "(get-value (%s))\n", strings.Join(names, " "))
	}

	r, err := c.z.Run(ctx, b.String())
	if err != nil {
		return "", nil, nil, err
	}
	if r.Status != solver.Sat {
		return r.Status, nil, nil, nil
	}
	values := replyValues(r)
	if !hasAll(values, names) {
		// The solver ran out of time while giving the model: a situation
		// without values reproduces nothing.
		return solver.Unknown, nil, nil, nil
	}
	var selected []int
	for i := range c.u.Rules {
		if values[selName(i)] == "true" {
			selected = append(selected, i)
		}
	}
	witness := map[string]string{}
	for _, v := range c.env {
		if val, ok := values[v.Name]; ok {
			witness[v.Name] = val
		}
	}
	return solver.Sat, witness, selected, nil
}

func (c *q) fixWitness(b *strings.Builder, witness map[string]string) {
	for _, v := range c.env {
		if val, ok := witness[v.Name]; ok {
			fmt.Fprintf(b, "(assert (= %s %s))\n", v.Name, val)
		}
	}
}

// unsatAt reports whether the rules in set have no common response at witness.
func (c *q) unsatAt(ctx context.Context, witness map[string]string, set []int) (solver.Status, error) {
	var b strings.Builder
	c.preamble(&b)
	declare(&b, c.env)
	declare(&b, c.sys)
	c.assumptions(&b)
	c.fixWitness(&b, witness)
	for _, t := range c.u.Fixed() {
		fmt.Fprintf(&b, "(assert %s)\n", t)
	}
	for _, i := range set {
		fmt.Fprintf(&b, "(assert %s) ; %s\n", c.u.Rules[i].Term, c.u.Rules[i].Rule.ID)
	}
	b.WriteString("(check-sat)\n")
	r, err := c.z.Run(ctx, b.String())
	if err != nil {
		return "", err
	}
	return r.Status, nil
}

// minimalCore shrinks the selected rules to a minimal set that still has no
// common response at witness. It returns nil on a solver unknown.
func (c *q) minimalCore(ctx context.Context, witness map[string]string, selected []int) ([]int, error) {
	core := append([]int{}, selected...)
	if st, err := c.unsatAt(ctx, witness, core); err != nil || st != solver.Unsat {
		if err == nil && st == solver.Sat {
			err = fmt.Errorf("internal: counterexample does not reproduce in unit %s", c.u.Name)
		}
		return nil, err
	}
	if shrunk, err := c.solverCore(ctx, func(b *strings.Builder) { c.fixWitness(b, witness) }, core); err != nil {
		return nil, err
	} else if shrunk != nil {
		if st, err := c.unsatAt(ctx, witness, shrunk); err != nil {
			return nil, err
		} else if st == solver.Unsat {
			core = shrunk
		}
	}
	for k := 0; k < len(core); {
		trial := append(append([]int{}, core[:k]...), core[k+1:]...)
		st, err := c.unsatAt(ctx, witness, trial)
		if err != nil {
			return nil, err
		}
		switch st {
		case solver.Unsat:
			core = trial
		case solver.Sat:
			k++
		default:
			return nil, nil
		}
	}
	return core, nil
}

// solverCore asserts the rules in set under extra constraints, with the
// fixed constraints computed for set, and returns the solver's unsat core, or
// nil when the query is not unsat.
func (c *q) solverCore(ctx context.Context, extra func(*strings.Builder), set []int) ([]int, error) {
	var b strings.Builder
	b.WriteString("(set-option :produce-unsat-cores true)\n")
	c.preamble(&b)
	declare(&b, c.env)
	declare(&b, c.sys)
	c.assumptions(&b)
	extra(&b)
	for _, t := range c.u.Fixed() {
		fmt.Fprintf(&b, "(assert %s)\n", t)
	}
	for _, i := range set {
		fmt.Fprintf(&b, "(assert (! %s :named |rule:%d|))\n", c.u.Rules[i].Term, i)
	}
	b.WriteString("(check-sat)\n(get-unsat-core)\n")
	r, err := c.z.Run(ctx, b.String())
	if err != nil || r.Status != solver.Unsat || len(r.Replies) == 0 {
		return nil, err
	}
	var core []int
	for _, a := range r.Replies[0].List {
		var i int
		if _, err := fmt.Sscanf(strings.Trim(a.Atom, "|"), "rule:%d", &i); err == nil {
			core = append(core, i)
		}
	}
	sort.Ints(core)
	return core, nil
}

func (c *q) describe(core []int, witness map[string]string) Conflict {
	out := Conflict{Unit: c.u.Name}
	mentioned := map[string]bool{}
	seen := map[string]bool{}
	for _, i := range core {
		f := c.u.Rules[i]
		if !seen[f.Rule.ID] {
			seen[f.Rule.ID] = true
			out.Rules = append(out.Rules, f.Rule.ID)
		}
		for _, v := range f.Vars {
			if !v.System {
				mentioned[v.Name] = true
			}
		}
	}
	sort.Strings(out.Rules)
	// Whether the operation runs depends on its permissions and preconditions
	// through its default, so what they read is part of the situation too.
	for _, f := range c.u.Rules {
		if f.Role != lower.Plain {
			for _, v := range f.Vars {
				if !v.System {
					mentioned[v.Name] = true
				}
			}
		}
	}
	out.before = map[string]string{}
	for _, v := range c.env {
		out.before[v.Label] = witness[v.Name]
		b := Binding{Name: v.Label, Value: Value(v, witness[v.Name])}
		out.Situation = append(out.Situation, b)
		if mentioned[v.Name] {
			out.Counterexample = append(out.Counterexample, b)
		}
	}
	return out
}

// Value renders a solver value for a variable: enum constructors lose their
// type prefix and negative ints are written plainly.
func Value(v *lower.Var, val string) string {
	if v.Attr != nil {
		val = strings.TrimPrefix(strings.Trim(val, "|"), v.Attr.QName()+":")
		val = strings.ReplaceAll(strings.ReplaceAll(val, "(- ", "-"), ")", "")
	}
	return val
}

// AssumptionsUnit names the unit that checks the assumptions together.
const AssumptionsUnit = "assumptions"

// assumptionUnit gathers every assumption as a rule of its own unit. What a
// requirement asserts is the response; what assumptions only read in their
// conditions is the situation. A conflict is a situation in which the
// requirements cannot all hold: "warn within 5 minutes of shutdown" and
// "never warn" conflict once shutdown can be 5 minutes away.
func assumptionUnit(prog *lower.Program) *lower.Unit {
	fs := prog.Assumed()
	if len(fs) == 0 {
		return nil
	}
	vars := map[string]*lower.Var{}
	for _, f := range fs {
		for _, v := range f.Vars {
			vars[v.Name] = v
		}
	}
	u := &lower.Unit{Name: AssumptionsUnit, Rules: fs}
	for _, v := range vars {
		u.Vars = append(u.Vars, v)
	}
	sort.Slice(u.Vars, func(i, j int) bool { return u.Vars[i].Label < u.Vars[j].Label })
	return u
}

// Vacuous is a rule whose condition can never hold given the unit's
// assumptions, so it constrains nothing and cannot conflict with anything.
// It is not a conflict, but it is almost always a mistranslation or a
// contradiction with the environment: a rule meant to apply never does.
type Vacuous struct {
	Unit    string   `json:"unit"`
	Rule    string   `json:"rule"`
	Because []string `json:"because"` // assumptions that exclude the condition; empty when it excludes itself
	// Avoided: the condition can hold, but the rule cannot hold there, so
	// the system meets it only by never being in that situation ("warn when
	// the shutdown deadline is near" against "never show a warning": the
	// deadline may then never come near while running). The state check
	// passes, since the system may keep out of a situation it controls.
	Avoided bool `json:"avoided,omitempty"`
}

// vacuous checks each rule's condition against the unit's assumptions,
// leaving out assumptions already reported as conflicting with each other:
// those exclude every condition and are reported once, as a conflict. In
// the state unit it also checks each invariant whose condition can hold
// against the assumptions with the invariant itself (Vacuous.Avoided).
func (c *q) vacuous(ctx context.Context, conflicting map[string]bool) ([]Vacuous, error) {
	var out []Vacuous
	for _, f := range c.u.Rules {
		cond := f.Condition()
		if !f.Home || cond == "" {
			continue
		}
		v, err := c.excluded(ctx, conflicting, f, cond, "")
		if err != nil {
			return nil, err
		}
		if v == nil && c.u.Op == nil && f.Rule.Kind == ir.Invariant {
			if v, err = c.excluded(ctx, conflicting, f, cond, f.Term); err != nil {
				return nil, err
			}
			if v != nil && len(v.Because) == 0 {
				v = nil // the rule contradicts its own condition: a translation slip, not the others' doing
			}
			if v != nil {
				v.Avoided = true
			}
		}
		if v != nil {
			out = append(out, *v)
		}
	}
	return out, nil
}

// excluded reports whether the unit's assumptions rule out cond, and which
// do. Given self (the rule's own term), the unit's other rules count too.
func (c *q) excluded(ctx context.Context, conflicting map[string]bool, f *lower.Formula, cond, self string) (*Vacuous, error) {
	var b strings.Builder
	b.WriteString("(set-option :produce-unsat-cores true)\n")
	c.preamble(&b)
	declare(&b, c.u.Vars)
	for i, a := range c.u.Assumptions {
		if !conflicting[a.Rule.ID] {
			fmt.Fprintf(&b, "(assert (! %s :named |a:%d|))\n", a.Term, i)
		}
	}
	if self != "" {
		// The other rules of the unit hold too: in the state unit a
		// statement that never shows a warning is a rule, not an assumption.
		fmt.Fprintf(&b, "(assert %s)\n", self)
		for i, g := range c.u.Rules {
			if g != f && !conflicting[g.Rule.ID] {
				fmt.Fprintf(&b, "(assert (! %s :named |r:%d|))\n", g.Term, i)
			}
		}
	}
	fmt.Fprintf(&b, "(assert (! %s :named |cond|))\n(check-sat)\n", cond)
	r, err := c.z.Run(ctx, b.String())
	if err != nil {
		return nil, err
	}
	if r.Status != solver.Unsat {
		return nil, nil
	}
	// Rare, so ask again for the core only now (z3 errors on a core after sat).
	if r, err = c.z.Run(ctx, b.String()+"(get-unsat-core)\n"); err != nil {
		return nil, err
	}
	v := &Vacuous{Unit: c.u.Name, Rule: f.Rule.ID}
	seen := map[string]bool{}
	usesCond := false
	if len(r.Replies) > 0 {
		for _, a := range r.Replies[0].List {
			usesCond = usesCond || a.Atom == "cond" || a.Atom == "|cond|"
			var i int
			id := ""
			if _, err := fmt.Sscanf(strings.Trim(a.Atom, "|"), "a:%d", &i); err == nil {
				id = c.u.Assumptions[i].Rule.ID
			} else if _, err := fmt.Sscanf(strings.Trim(a.Atom, "|"), "r:%d", &i); err == nil {
				id = c.u.Rules[i].Rule.ID
			}
			if id != "" && !seen[id] {
				seen[id] = true
				v.Because = append(v.Because, id)
			}
		}
	}
	if !usesCond {
		return nil, nil // the assumptions alone are inconsistent (with the unit's ranges); not this rule's doing
	}
	sort.Strings(v.Because)
	return v, nil
}

// setAside adds the rules of conflicts to the set checked without.
func setAside(set map[string]bool, cs []Conflict) {
	for _, c := range cs {
		for _, r := range c.Rules {
			set[r] = true
		}
	}
}

// excludedBy returns the spec statements' invariants (not proposed world
// facts) that make some rule of u vacuous.
func excludedBy(u *lower.Unit, vac []Vacuous) map[string]bool {
	isStatement := map[string]bool{}
	for _, a := range u.Assumptions {
		if a.Rule != nil && a.Rule.StatementID != "" {
			isStatement[a.Rule.ID] = true
		}
	}
	out := map[string]bool{}
	for _, v := range vac {
		for _, id := range v.Because {
			if isStatement[id] {
				out[id] = true
			}
		}
	}
	return out
}

func hasConflict(cs []Conflict, rules []string) bool {
	key := strings.Join(slices.Sorted(slices.Values(rules)), ",")
	for _, c := range cs {
		if strings.Join(slices.Sorted(slices.Values(c.Rules)), ",") == key {
			return true
		}
	}
	return false
}
