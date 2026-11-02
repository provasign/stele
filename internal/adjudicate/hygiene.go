package adjudicate

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/translate"
)

// Hygiene is what the structural pass found: defects that need no model
// and no solver to see.
type Hygiene struct {
	// Restated are statements stated more than once in one file under the
	// same title, with different text: usually an old version left next
	// to its replacement.
	Restated []Restated `json:"restated,omitempty"`
	// Repeated are titles stated more than once in one file with the same
	// text.
	Repeated []Restated `json:"repeated,omitempty"`
	// Misplaced are directories inside the specs tree that hold no spec of
	// their own, only nested ones (OpenSpec: a change folder left in specs/).
	Misplaced []string `json:"misplaced,omitempty"`
	// Unknown are names a statement writes as code that appear nowhere in
	// the code.
	Unknown []UnknownName `json:"unknown_names,omitempty"`
}

// Restated is one title with its copies.
type Restated struct {
	File  string   `json:"file"`
	Title string   `json:"title"`
	IDs   []string `json:"statement_ids"`
}

// UnknownName is a code-like name from a statement that the code lacks.
type UnknownName struct {
	StatementID string `json:"statement_id"`
	Name        string `json:"name"`
}

var lineSuffix = regexp.MustCompile(`:L\d+$`)

// codeName matches what an author writes as a code name: snake_case with
// at least one underscore, a dotted key, or a long CLI flag.
var codeName = regexp.MustCompile(`^([a-z][a-z0-9]*(_[a-z0-9]+)+|[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+|--[a-z][a-z0-9-]+)$`)

// CheckHygiene runs the structural pass over stmts from repo.
func CheckHygiene(repo string, stmts []translate.Statement) (*Hygiene, error) {
	h := &Hygiene{}
	type key struct{ file, title string }
	groups := map[key][]translate.Statement{}
	var order []key
	for _, s := range stmts {
		file, _, _ := strings.Cut(s.Source, "#")
		k := key{file, lineSuffix.ReplaceAllString(s.ID, "")}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], s)
	}
	for _, k := range order {
		g := groups[k]
		if len(g) < 2 {
			continue
		}
		r := Restated{File: k.file, Title: k.title}
		texts := map[string]bool{}
		for _, s := range g {
			r.IDs = append(r.IDs, s.ID)
			texts[norm(s.Prose)] = true
		}
		if len(texts) > 1 {
			h.Restated = append(h.Restated, r)
		} else {
			h.Repeated = append(h.Repeated, r)
		}
	}

	// OpenSpec: a directory directly under openspec/specs is a capability
	// only if it has its own spec.md.
	root := filepath.Join(repo, "openspec", "specs")
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, e.Name(), "spec.md")); err == nil {
				continue
			}
			h.Misplaced = append(h.Misplaced, "openspec/specs/"+e.Name())
		}
	}

	var sources []string
	for _, s := range stmts {
		sources = append(sources, s.Source)
	}
	exclude := SpecDirs(sources)
	files, err := trackedNames(repo)
	if err != nil {
		return nil, err
	}
	checked := map[string]bool{}
	present := map[string]bool{}
	for _, s := range stmts {
		for _, n := range QuotedNames(s.Prose) {
			if !codeName.MatchString(n) {
				continue
			}
			if !checked[n] {
				checked[n] = true
				found, err := nameInCode(repo, n, exclude, files)
				if err != nil {
					return nil, err
				}
				present[n] = found
			}
			if !present[n] {
				h.Unknown = append(h.Unknown, UnknownName{s.ID, n})
			}
		}
	}
	sort.SliceStable(h.Unknown, func(i, j int) bool { return h.Unknown[i].Name < h.Unknown[j].Name })
	return h, nil
}

// nameInCode reports whether a code name from a statement exists in the
// project: as text outside the specs, a long flag also without its dashes
// (flag libraries declare "name", not "--name"), or as a tracked file name.
func nameInCode(repo, name string, exclude []string, files map[string]bool) (bool, error) {
	if files[name] {
		return true, nil
	}
	probes := []string{name}
	if strings.HasPrefix(name, "--") {
		probes = append(probes, "\""+strings.TrimPrefix(name, "--")+"\"")
	}
	for _, p := range probes {
		hits, err := grep(repo, p, exclude)
		if err != nil {
			return false, err
		}
		if len(hits) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// trackedNames is the set of base names of the repository's tracked files.
func trackedNames(repo string) (map[string]bool, error) {
	out, err := exec.Command("git", "-C", repo, "ls-files").Output()
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, f := range strings.Split(string(out), "\n") {
		if f != "" {
			names[path.Base(f)] = true
		}
	}
	return names, nil
}
