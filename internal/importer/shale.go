package importer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/translate"
)

// RetiredIntentsFile lists the Shale intents the owner said a later one
// replaces, relative to the repository root. Shale's evidence is
// append-only, so a request that is out of date is retired here, in
// Stele's own folder, and never edited in .shale/.
const RetiredIntentsFile = ".stele/retired-intents.yaml"

// RetiredIntent is one retired intent: which statement, who said so, when,
// and the statement that replaces it.
type RetiredIntent struct {
	ID string `yaml:"id"`
	By string `yaml:"by,omitempty"`
	At string `yaml:"at,omitempty"`
	// For is the statement whose conflict the owner settled by retiring
	// this one.
	For string `yaml:"for,omitempty"`
	// Why is set when the owner retired the request for another reason
	// than a later one replacing it: "not a requirement", a one-off task.
	Why string `yaml:"why,omitempty"`
}

// Shale reads the requests a project's coding agents declared with
// `shale intent`: one statement per task, `intent/<session id>` and
// `intent/<session id>-c<n>` for a session's later tasks, whose prose is the
// intent's title, then its body. A completion (shale done) closes a task. Finalized sessions are read
// from .shale/<id>.yaml; a session still open, from the latest intent in
// .shale/local/<id>.jsonl, so a request is checked before the agent edits.
// A session with no intent declared says nothing, and an intent listed in
// RetiredIntentsFile is skipped. A statement's source is its session's
// evidence file, .shale/<id>.yaml, relative to the repository root, also
// while the session is open and the file is still to be written.
//
// A request declared after its session's evidence was finalized (the owner
// asked for something new in the same conversation) is a request of its
// own: Shale folds it into a continuation, .shale/<id>-c2.yaml and on, and
// it reads as `intent/<id>-c2`.
func Shale(repo string) ([]translate.Statement, error) {
	return shale(repo, false)
}

// ShaleRetired reads the requests listed in RetiredIntentsFile that are
// still in the evidence, as Shale reads them otherwise.
func ShaleRetired(repo string) ([]translate.Statement, error) {
	return shale(repo, true)
}

func shale(repo string, retiredOnly bool) ([]translate.Statement, error) {
	dir := filepath.Join(repo, ".shale")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("no .shale/ under %s: %w", repo, ErrNoSpecs)
	}
	retired := map[string]bool{}
	rs, err := RetiredIntents(repo)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		retired[r.ID] = true
	}
	type intent struct {
		session, title, body, source string
		at                           time.Time
	}
	found := map[string]intent{}
	finalized := map[string]time.Time{} // every evidence file, with or without a request
	files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	for _, f := range files {
		if filepath.Base(f) == "config.yaml" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var ev struct {
			ID          string    `yaml:"id"`
			FinalizedAt time.Time `yaml:"finalized_at"`
			Intent      *struct {
				Title      string    `yaml:"title"`
				Body       string    `yaml:"body"`
				DeclaredAt time.Time `yaml:"declared_at"`
			} `yaml:"intent"`
		}
		if err := yaml.Unmarshal(src, &ev); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if ev.ID == "" {
			continue
		}
		finalized[ev.ID] = ev.FinalizedAt
		if ev.Intent == nil || strings.TrimSpace(ev.Intent.Title) == "" {
			continue
		}
		if ev.FinalizedAt.IsZero() {
			finalized[ev.ID] = ev.Intent.DeclaredAt
		}
		found[ev.ID] = intent{ev.ID, ev.Intent.Title, ev.Intent.Body, rel(repo, f), ev.Intent.DeclaredAt}
	}
	// Open sessions: the evidence is folded into .shale/<id>.yaml only when
	// the session ends, and until then the finalized file, if any, is older.
	logs, _ := filepath.Glob(filepath.Join(dir, "local", "*.jsonl"))
	for _, f := range logs {
		session := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		tasks, err := logTasks(f)
		if err != nil {
			return nil, err
		}
		// Shale's log holds only what it has not folded yet: a task still
		// open when a commit recorded the closed ones stays, and may be
		// older than their files.
		_, finalizedAlready := finalized[session]
		// Shale writes one file per task, <session>.yaml and then the first
		// continuation names free; each task takes its name here as there,
		// so a request keeps its ID when the session is finalized.
		next := func() string {
			for n := 2; ; n++ {
				if c := fmt.Sprintf("%s-c%d", session, n); !hasKey(finalized, c) {
					return c
				}
			}
		}
		for i, t := range tasks {
			id := session
			if i > 0 || finalizedAlready {
				id = next()
			}
			finalized[id] = time.Time{} // taken, for the tasks after it
			if strings.TrimSpace(t.title) == "" {
				continue // no request
			}
			// The source is where the evidence lands when the session ends,
			// so the rule's provenance does not move then.
			found[id] = intent{id, t.title, t.body, ".shale/" + id + ".yaml", t.at}
		}
	}
	var out []translate.Statement
	for _, in := range found {
		id := "intent/" + in.session
		if retired[id] != retiredOnly {
			continue
		}
		prose := strings.TrimSpace(in.title)
		if b := strings.TrimSpace(in.body); b != "" {
			prose += "\n" + b
		}
		s := translate.Statement{ID: id, Source: in.source, Prose: prose}
		if !in.at.IsZero() {
			s.Introduced = in.at.UTC().Format("2006-01-02")
		}
		out = append(out, s)
	}
	// In the order the requests were made, so the newer of two that clash
	// reads as the change.
	sort.Slice(out, func(i, j int) bool {
		a, b := found[strings.TrimPrefix(out[i].ID, "intent/")].at, found[strings.TrimPrefix(out[j].ID, "intent/")].at
		if !a.Equal(b) {
			return a.Before(b)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// logTask is one task of an open session: its latest request.
type logTask struct {
	title, body string
	at          time.Time
}

// logTasks reads a session's event log as Shale folds it: a completion
// (shale done) closes a task, and a request declared after it starts the
// next one; a request declared again before the completion restates the
// task, and the latest wording counts. A task with no request has an empty
// title but still takes its place, as Shale writes a file for it.
func logTasks(path string) ([]logTask, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tasks := []logTask{{}}
	closed := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev struct {
			Kind  string    `json:"kind"`
			At    time.Time `json:"at"`
			Title string    `json:"title"`
			Body  string    `json:"body"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue // a torn or foreign line says nothing about the request
		}
		switch ev.Kind {
		case "completion", "session_close":
			// shale done, or the agent's session ending for good.
			closed = true
		case "intent":
			if closed {
				tasks, closed = append(tasks, logTask{}), false
			}
			tasks[len(tasks)-1] = logTask{ev.Title, ev.Body, ev.At}
		}
	}
	return tasks, sc.Err()
}

// RetiredIntents reads RetiredIntentsFile; none is no error.
func RetiredIntents(repo string) ([]RetiredIntent, error) {
	src, err := os.ReadFile(filepath.Join(repo, RetiredIntentsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rs []RetiredIntent
	if err := yaml.Unmarshal(src, &rs); err != nil {
		return nil, fmt.Errorf("%s: %w", RetiredIntentsFile, err)
	}
	return rs, nil
}

// RetireIntents adds intents to RetiredIntentsFile; one already retired is
// left as it was.
func RetireIntents(repo string, add []RetiredIntent) error {
	rs, err := RetiredIntents(repo)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, r := range rs {
		have[r.ID] = true
	}
	for _, r := range add {
		if !have[r.ID] {
			rs, have[r.ID] = append(rs, r), true
		}
	}
	src, err := yaml.Marshal(rs)
	if err != nil {
		return err
	}
	path := filepath.Join(repo, RetiredIntentsFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	head := "# Requests the owner set aside: replaced by a later one (stele change answer ... update_old), or not a requirement (... task).\n"
	return os.WriteFile(path, append([]byte(head), src...), 0o644)
}

func hasKey[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}

func rel(repo, path string) string {
	r, err := filepath.Rel(repo, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(r)
}
