package gate

import (
	"sort"
	"strings"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
)

// Open conflict states (docs/08 W5, open conflicts and the ratchet).
const (
	OpenTolerated = "open"     // still found, and tolerated
	OpenStale     = "stale"    // a pinned rule changed; tolerates nothing
	OpenResolved  = "resolved" // no longer found: close the record
	OpenClosed    = "closed"
)

// OpenStatus is what one check found for one open conflict.
type OpenStatus struct {
	ID       string   `json:"id"`
	Rules    []string `json:"rules"`
	Response string   `json:"response,omitempty"`
	Since    string   `json:"since,omitempty"`
	Status   string   `json:"status"`
}

func setKey(rules []string, response string) string {
	rs := append([]string{}, rules...)
	sort.Strings(rs)
	return response + "|" + strings.Join(rs, ",")
}

// CheckOptions exempts only live, current open conflicts from enumeration's
// budget. Callers that apply Ratchet must use these options when checking;
// the conflicts remain in the result until the ratchet validates them. Units
// already answered come from the persisted cache, when there is one.
func CheckOptions(reg *ir.Registry) checker.Options {
	opt := checker.Options{Cache: checker.DefaultCache()}
	for _, o := range reg.Open {
		if !o.Closed && !o.Stale && o.Response == "" {
			opt.KnownConflicts = append(opt.KnownConflicts, o.RuleIDs())
		}
	}
	return opt
}

// Ratchet removes from res every conflict and dead response whose rule set is
// exactly an open conflict's pinned set (at the pinned versions), and
// recomputes the verdict. Every other conflict still fails the check, so the
// count of conflicts can only stay or fall (D-044). The open conflicts come
// back oldest first.
func Ratchet(res *checker.Result, reg *ir.Registry) []OpenStatus {
	tolerated := map[string]*ir.OpenConflict{}
	for _, o := range reg.Open {
		if !o.Closed && !o.Stale {
			tolerated[setKey(o.RuleIDs(), o.Response)] = o
		}
	}
	found := map[string]bool{}
	var conflicts []checker.Conflict
	for _, c := range res.Conflicts {
		if o := tolerated[setKey(c.Rules, "")]; o != nil {
			found[o.ID] = true
			continue
		}
		conflicts = append(conflicts, c)
	}
	var dead []checker.DeadResponse
	for _, d := range res.Dead {
		if o := tolerated[setKey(d.Rules, d.Response)]; o != nil {
			found[o.ID] = true
			continue
		}
		dead = append(dead, d)
	}
	res.Conflicts, res.Dead = conflicts, dead
	if res.Verdict == checker.Fail && len(conflicts) == 0 && len(dead) == 0 {
		res.Verdict = checker.Pass
		if len(res.Undecided) > 0 {
			res.Verdict = checker.Unknown
		}
	}
	var out []OpenStatus
	for _, o := range reg.Open {
		s := OpenStatus{ID: o.ID, Response: o.Response, Since: o.Since, Status: OpenTolerated}
		for _, p := range o.Rules {
			s.Rules = append(s.Rules, p.String())
		}
		switch {
		case o.Closed:
			s.Status = OpenClosed
		case o.Stale:
			s.Status = OpenStale
		case !found[o.ID] && len(res.Undecided) == 0:
			s.Status = OpenResolved
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Since < out[j].Since })
	return out
}

// Pinned removes from res every dead response that is one unconditional
// invariant fixing the attribute it names: "the deletion mode is disabled"
// makes the mode's other values impossible, which is what the rule says,
// not a defect. They are returned so a report can still list them.
func Pinned(res *checker.Result, reg *ir.Registry) []checker.DeadResponse {
	var pinned, dead []checker.DeadResponse
	for _, d := range res.Dead {
		if d.Unit == "state" && len(d.Rules) == 1 && fixes(reg, d.Rules[0], d.Response) {
			pinned = append(pinned, d)
			continue
		}
		dead = append(dead, d)
	}
	res.Dead = dead
	if res.Verdict == checker.Fail && len(res.Conflicts) == 0 && len(dead) == 0 {
		res.Verdict = checker.Pass
		if len(res.Undecided) > 0 {
			res.Verdict = checker.Unknown
		}
	}
	return pinned
}

// fixes reports whether the rule is an invariant that holds in every state
// and reads the attribute of a response "Entity.attr = value".
func fixes(reg *ir.Registry, rule, response string) bool {
	x := reg.Rule(rule)
	attr, _, ok := strings.Cut(response, " = ")
	if x == nil || !ok || x.Kind != ir.Invariant || x.When != nil || x.On != "" || x.Assert == nil {
		return false
	}
	subject, err := reg.SubjectOf(x)
	if err != nil {
		return false
	}
	for _, a := range reg.Reads(x.Assert, subject) {
		if a.QName() == attr {
			return true
		}
	}
	return false
}
