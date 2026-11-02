// Package specsync detects saved-spec drift without calling a model or
// changing approved rules. The CLI and language server share this engine.
package specsync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/provasign/stele/internal/adopt"
	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/score"
	"github.com/provasign/stele/internal/translate"
	"gopkg.in/yaml.v3"
)

type Options struct {
	Root, Format, Snapshot, Registry string
	AnyCase                          bool
}

func (o Options) Validate() error {
	if o.Root == "" || o.Snapshot == "" || o.Registry == "" {
		return fmt.Errorf("spec root, accepted snapshot and registry are required")
	}
	switch o.Format {
	case "markdown", "speckit", "openspec", "kiro", "shale":
	default:
		return fmt.Errorf("format must be markdown, speckit, openspec, kiro or shale")
	}
	root, _ := filepath.Abs(o.Root)
	reg, _ := filepath.Abs(o.Registry)
	snapshot, _ := filepath.Abs(o.Snapshot)
	if root == reg || within(snapshot, reg) {
		return fmt.Errorf("spec root must differ from the registry, and snapshot must be outside the registry")
	}
	return nil
}

type Diagnostic struct {
	File      string   `json:"file"`
	Line      int      `json:"line"` // one-based; adapters clamp to the current document
	Code      string   `json:"code"`
	Message   string   `json:"message"`
	Statement string   `json:"statement,omitempty"`
	Rules     []string `json:"rules,omitempty"`
}

type Result struct {
	InSync      bool                  `json:"in_sync"`
	RootMissing bool                  `json:"root_missing,omitempty"`
	Changes     *adopt.SyncReport     `json:"changes"`
	Pending     []translate.Statement `json:"pending"`
	StaleRules  []string              `json:"stale_rules"`
	OrphanRules []string              `json:"orphan_rules"`
	Diagnostics []Diagnostic          `json:"diagnostics"`
	// Next is the command that checks the pending statements, for the
	// coding agent; the caller fills it from the project's settings.
	Next string `json:"next,omitempty"`
}

func Evaluate(o Options) (*Result, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	old, err := translate.LoadStatements(o.Snapshot)
	if err != nil {
		return nil, err
	}
	reg, err := ir.LoadDir(o.Registry)
	if err != nil {
		return nil, err
	}
	var current []translate.Statement
	exclude := []string{o.Registry, o.Snapshot}
	switch o.Format {
	case "markdown":
		current, err = importer.Markdown(o.Root, importer.MarkdownOptions{AnyCase: o.AnyCase, Exclude: exclude})
	case "speckit":
		current, err = importer.SpecKit(o.Root, exclude...)
	case "openspec":
		current, err = importer.OpenSpec(o.Root, false, exclude...)
	case "kiro":
		current, err = importer.Kiro(o.Root, exclude...)
	case "shale":
		current, err = importer.Shale(o.Root)
	}
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !errors.Is(err, importer.ErrNoSpecs) && !missing {
		return nil, err
	}
	changes := adopt.Sync(old, current, reg)
	ApprovedAsWritten(changes, current, reg)
	r := &Result{Changes: changes, RootMissing: missing, Pending: []translate.Statement{}, StaleRules: []string{}, OrphanRules: []string{}, Diagnostics: []Diagnostic{}}
	if missing {
		r.Diagnostics = append(r.Diagnostics, Diagnostic{File: o.Snapshot, Line: 1, Code: "spec.root_missing", Message: "Configured spec directory is absent; linked statements are reported missing, with no rules retired automatically."})
	}
	byID := map[string]translate.Statement{}
	before := map[string]translate.Statement{}
	for _, s := range current {
		byID[s.ID] = s
	}
	for _, s := range old {
		before[s.ID] = s
	}
	want := map[string]bool{}
	add := func(s translate.Statement, code, message string, rules []string) {
		file, line := source(s.Source)
		r.Diagnostics = append(r.Diagnostics, Diagnostic{File: filepath.Join(o.Root, filepath.FromSlash(file)), Line: line, Code: code, Message: message, Statement: s.ID, Rules: rules})
	}
	for _, id := range changes.Added {
		want[id] = true
		add(byID[id], "spec.added", "New requirement needs translation and review.", nil)
	}
	for _, id := range changes.Changed {
		want[id] = true
		add(byID[id], "spec.changed", "Requirement changed; refresh its rules and bump their versions.", changes.RulesOf[id])
	}
	for _, s := range current {
		if want[s.ID] {
			r.Pending = append(r.Pending, s)
		}
	}
	for oldID, newID := range changes.Moved {
		add(byID[newID], "spec.moved", "Requirement moved from "+oldID+"; update provenance, preserving rule IDs and versions.", nil)
	}
	for _, id := range changes.Removed {
		d := Diagnostic{File: o.Snapshot, Line: 1, Code: "spec.removed", Statement: id, Message: "Requirement removed from " + before[id].Source + "; review retirement of its linked rules."}
		for _, ruleID := range changes.Retire {
			x := reg.Rule(ruleID)
			if score.StatementFor(ruleID, old, reg) == id {
				d.Rules = append(d.Rules, ruleID)
				if len(d.Rules) == 1 {
					d.File, d.Line = ruleLocation(x.File)
				}
			}
		}
		r.Diagnostics = append(r.Diagnostics, d)
	}
	for id, targets := range changes.Ambiguous {
		d := Diagnostic{File: o.Snapshot, Line: 1, Code: "spec.ambiguous", Statement: id, Message: "Ambiguous statement identity; do not retire or remap rules automatically: " + strings.Join(targets, ", ")}
		r.Diagnostics = append(r.Diagnostics, d)
	}
	for _, x := range reg.Rules {
		if !x.Live() {
			continue
		}
		id := score.StatementFor(x.ID, old, reg)
		if c, ok := byID[x.StatementID]; ok && x.Source == c.Source && x.StatementHash == translate.TextHash(c.Prose) {
			id = c.ID
		} else if moved, ok := changes.Moved[id]; ok {
			id = moved
		}
		c, ok := byID[id]
		if !ok {
			if x.StatementID != "" && (x.TextHash != "" || x.Prose != "") {
				r.OrphanRules = append(r.OrphanRules, x.ID)
				if !slices.Contains(changes.Retire, x.ID) {
					file, line := ruleLocation(x.File)
					r.Diagnostics = append(r.Diagnostics, Diagnostic{File: file, Line: line, Code: "rule.orphaned", Statement: x.StatementID, Rules: []string{x.ID}, Message: "Live rule's source statement is absent; resolve its move or review retirement. Advancing the source snapshot does not retire the rule."})
				}
			}
			continue
		}
		hash := x.TextHash
		if hash == "" && x.Prose != "" {
			hash = translate.TextHash(x.Prose)
		}
		if hash != "" && hash != translate.TextHash(c.Prose) {
			r.StaleRules = append(r.StaleRules, x.ID)
			file, line := ruleLocation(x.File)
			r.Diagnostics = append(r.Diagnostics, Diagnostic{File: file, Line: line, Code: "rule.stale", Statement: c.ID, Rules: []string{x.ID}, Message: "Rule was translated from older prose; provenance updates alone do not refresh its meaning."})
		}
	}
	// Location-only changes also need a mechanical provenance update, even if
	// an explicit requirement ID and its text are unchanged.
	if len(changes.Sources) > 0 && len(changes.Added)+len(changes.Changed)+len(changes.Moved) == 0 {
		for _, update := range changes.Sources {
			if s, ok := byID[update.StatementID]; ok {
				add(s, "spec.provenance", "Rule source location or imported identity needs updating: run `stele sync --apply` (mechanical: rule IDs, versions and meaning are kept).", nil)
			}
		}
	}
	pendingChanges(o, reg, r, changes)
	sort.Strings(r.StaleRules)
	sort.Strings(r.OrphanRules)
	sort.Slice(r.Diagnostics, func(i, j int) bool {
		a, b := r.Diagnostics[i], r.Diagnostics[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Statement < b.Statement
	})
	r.InSync = !missing && len(changes.Added)+len(changes.Changed)+len(changes.Removed)+len(changes.Moved)+len(changes.Ambiguous)+len(changes.Sources)+len(r.StaleRules)+len(r.OrphanRules) == 0
	return r, nil
}

func source(s string) (string, int) {
	if i := strings.LastIndex(s, "#L"); i >= 0 {
		if n, err := strconv.Atoi(s[i+2:]); err == nil {
			return s[:i], n
		}
	}
	return s, 1
}
func ruleLocation(s string) (string, int) {
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		if n, err := strconv.Atoi(s[i+1:]); err == nil {
			return s[:i], n
		}
	}
	return s, 1
}
func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// WritePending atomically replaces the queue; it never advances the accepted
// snapshot, updates rule provenance or retires a rule.
func WritePending(path string, r *Result, o Options) error {
	ext := strings.ToLower(filepath.Ext(path))
	if (ext != ".yaml" && ext != ".yml") || filepath.Base(path) == "stele.yaml" {
		return fmt.Errorf("pending queue must be a .yaml/.yml file, not a spec or stele.yaml")
	}
	abs, _ := filepath.Abs(path)
	old, _ := filepath.Abs(o.Snapshot)
	reg, _ := filepath.Abs(o.Registry)
	if abs == old || within(abs, reg) {
		return fmt.Errorf("pending queue must not overwrite the snapshot or live registry")
	}
	data, err := yaml.Marshal(r.Pending)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".stele-pending-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Watch reacts to stable content changes, including atomic editor saves,
// deletion of the last file, baseline updates and external registry changes.
// An error never becomes a clean status. No model or solver is invoked.
func Watch(ctx context.Context, o Options, interval time.Duration, emit func(*Result, error)) error {
	if interval <= 0 {
		return fmt.Errorf("watch interval must be positive")
	}
	if err := o.Validate(); err != nil {
		return err
	}
	lastResult, previous, accepted := "", "", ""
	evaluate := func() {
		before, fingerprintErr := fingerprint(o)
		if fingerprintErr != nil {
			emit(nil, fingerprintErr)
			return
		}
		r, err := Evaluate(o)
		after, fingerprintErr := fingerprint(o)
		if fingerprintErr != nil {
			emit(nil, fingerprintErr)
			return
		}
		if before != after {
			previous = after
			return
		} // retry a concurrent save
		previous, accepted = after, after // bind the result before calling the client
		data, _ := json.Marshal(struct {
			Result *Result
			Error  string
		}{r, errorText(err)})
		if string(data) != lastResult {
			lastResult = string(data)
			emit(r, err)
		}
	}
	evaluate()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			now, err := fingerprint(o)
			if err != nil {
				emit(nil, err)
				continue
			}
			if now != previous {
				previous = now
				continue
			}
			if now != accepted {
				evaluate()
			}
		}
	}
}

func errorText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
func fingerprint(o Options) (string, error) {
	h := sha256.New()
	root := o.Root
	if o.Format == "speckit" {
		root = filepath.Join(root, "specs")
	}
	if o.Format == "openspec" {
		root = filepath.Join(root, "openspec", "specs")
	}
	if o.Format == "kiro" {
		root = filepath.Join(root, ".kiro", "specs")
	}
	if o.Format == "shale" {
		root = filepath.Join(root, ".shale")
	}
	for _, item := range []struct {
		path     string
		registry bool
	}{{root, false}, {o.Registry, true}, {o.Snapshot, true}} {
		err := filepath.WalkDir(item.path, func(path string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(h, "missing:%s\n", path)
				return nil
			}
			if err != nil {
				return err
			}
			if !item.registry {
				abs, _ := filepath.Abs(path)
				reg, _ := filepath.Abs(o.Registry)
				if within(abs, reg) {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() && path != item.path && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "vendor" || d.Name() == "dist" || d.Name() == "target") {
					return filepath.SkipDir
				}
			}
			if d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if path != o.Snapshot && ((!item.registry && ext != ".md") || (item.registry && ext != ".yaml" && ext != ".yml")) {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\x00%d\x00", path, len(data))
			h.Write(data)
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// pendingChanges drops, in an OpenSpec project, the stale and orphaned rules
// an active change accounts for, with their diagnostics. An approved change
// lives in openspec/changes/ until it is archived: the registry already
// holds its rules while openspec/specs/ still has the old text, so a rule
// that matches the specs with the active changes applied is current.
func pendingChanges(o Options, reg *ir.Registry, r *Result, changes *adopt.SyncReport) {
	if o.Format != "openspec" || len(r.StaleRules)+len(r.OrphanRules)+len(changes.Sources) == 0 {
		return
	}
	truth, err := importer.OpenSpec(o.Root, false)
	if err != nil {
		return
	}
	ids, err := importer.OpenSpecChanges(o.Root)
	if err != nil {
		return
	}
	inChange := map[string]bool{} // statements an active change adds or edits
	for _, id := range ids {
		if d, err := importer.OpenSpecChange(o.Root, id); err == nil {
			truth, _ = d.Apply(truth)
			for _, s := range append(append([]translate.Statement{}, d.Added...), d.Modified...) {
				inChange[s.ID] = true
			}
		}
	}
	// A rule of an active change points at the change's own file until the
	// change is archived: that is its provenance, not an update to make.
	dropped := false
	for rule, u := range changes.Sources {
		if inChange[u.StatementID] {
			delete(changes.Sources, rule)
			dropped = true
		}
	}
	text := map[string]string{}
	for _, s := range truth {
		text[s.ID] = translate.TextHash(s.Prose)
	}
	current := map[string]bool{}
	for _, id := range append(append([]string{}, r.StaleRules...), r.OrphanRules...) {
		if x := reg.Rule(id); x != nil {
			if h, ok := text[x.StatementID]; ok && (x.TextHash == "" || x.TextHash == h) {
				current[id] = true
			}
		}
	}
	if len(current) == 0 && !dropped {
		return
	}
	keep := func(ids []string) []string {
		out := []string{}
		for _, id := range ids {
			if !current[id] {
				out = append(out, id)
			}
		}
		return out
	}
	r.StaleRules, r.OrphanRules = keep(r.StaleRules), keep(r.OrphanRules)
	ds := []Diagnostic{}
	for _, d := range r.Diagnostics {
		if (d.Code == "rule.stale" || d.Code == "rule.orphaned") && len(d.Rules) == 1 && current[d.Rules[0]] {
			continue
		}
		if d.Code == "spec.provenance" && inChange[d.Statement] {
			continue
		}
		ds = append(ds, d)
	}
	r.Diagnostics = ds
}

// ApprovedAsWritten drops from changes the statements the snapshot calls
// added or changed but the registry already covers: every live rule of the
// statement was translated from its current text and approved for it (an
// OpenSpec change archived after its approval, say). The snapshot lags
// behind such a statement; its meaning was already reviewed.
func ApprovedAsWritten(changes *adopt.SyncReport, current []translate.Statement, reg *ir.Registry) {
	text := map[string]string{}
	for _, s := range current {
		text[s.ID] = translate.TextHash(s.Prose)
	}
	covered := func(id string) bool {
		found := false
		for _, x := range reg.Rules {
			if x.StatementID != id || !x.Live() {
				continue
			}
			if !x.Approved() || x.TextHash != text[id] {
				return false
			}
			found = true
		}
		return found
	}
	keep := func(ids []string) []string {
		out := []string{}
		for _, id := range ids {
			if !covered(id) {
				out = append(out, id)
			}
		}
		return out
	}
	changes.Added, changes.Changed = keep(changes.Added), keep(changes.Changed)
}
