package importer

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/translate"
)

// Delta is one OpenSpec change folder (openspec/changes/<id>): the
// requirements it adds, modifies, removes and renames, by capability. IDs
// are `<capability>/<title>`, as OpenSpec reads them; sources point into
// the change folder.
type Delta struct {
	Change   string
	Added    []translate.Statement
	Modified []translate.Statement
	Removed  []string
	// Renamed maps an old statement ID to its new one. OpenSpec applies
	// renames first, so a MODIFIED block names the requirement by its new
	// title.
	Renamed map[string]string
	titles  map[string]string // new title of each renamed ID
}

var (
	deltaHead = regexp.MustCompile(`^##\s+(ADDED|NEW|MODIFIED|REMOVED|RENAMED)\s+Requirements\s*$`)
	renameRe  = regexp.MustCompile("^\\s*-\\s*(FROM|TO):\\s*`?(?:###\\s+)?Requirement:\\s*(.+?)`?\\s*$")
)

// OpenSpecChanges lists the active change folders of repo (not archived).
func OpenSpecChanges(repo string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(repo, "openspec", "changes"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "archive" {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// OpenSpecChange reads the delta specs of one change folder.
func OpenSpecChange(repo, change string) (*Delta, error) {
	root := filepath.Join(repo, "openspec", "changes", change, "specs")
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "spec.md" {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("change %s: %w", change, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("change %s has no specs/**/spec.md: %w", change, ErrNoSpecs)
	}
	sort.Strings(files)
	d := &Delta{Change: change, Renamed: map[string]string{}, titles: map[string]string{}}
	for _, f := range files {
		rel, err := filepath.Rel(repo, f)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		capability := strings.TrimSuffix(strings.TrimPrefix(rel, "openspec/changes/"+change+"/specs/"), "/spec.md")
		if err := d.read(f, rel, capability); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// read adds one delta spec file to d.
func (d *Delta) read(path, rel, capability string) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	section := ""
	var cur *requirement
	var body []string
	var from string
	flush := func() {
		if cur == nil {
			return
		}
		for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
			body = body[:len(body)-1]
		}
		text := "Requirement: " + cur.title
		if b := strings.TrimSpace(strings.Join(body, "\n")); b != "" {
			text += "\n" + strings.ReplaceAll(b, "**", "")
		}
		s := translate.Statement{ID: capability + "/" + cur.title, Source: fmt.Sprintf("%s#L%d", rel, cur.line), Prose: text}
		switch section {
		case "ADDED", "NEW":
			d.Added = append(d.Added, s)
		case "MODIFIED":
			d.Modified = append(d.Modified, s)
		case "REMOVED":
			d.Removed = append(d.Removed, s.ID)
		}
		cur, body = nil, nil
	}
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if m := deltaHead.FindStringSubmatch(line); m != nil {
			flush()
			section = m[1]
			continue
		}
		if section == "RENAMED" {
			if m := renameRe.FindStringSubmatch(line); m != nil {
				if m[1] == "FROM" {
					from = m[2]
				} else if from != "" {
					d.Renamed[capability+"/"+from] = capability + "/" + m[2]
					d.titles[capability+"/"+from] = m[2]
					from = ""
				}
			}
			continue
		}
		if m := requirementHead.FindStringSubmatch(line); m != nil {
			flush()
			if section != "" {
				cur = &requirement{title: m[1], line: n}
			}
			continue
		}
		if sectionHead.MatchString(line) {
			flush()
			continue
		}
		if cur != nil && !IsNote(line) {
			body = append(body, line)
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Apply returns the statements the specs will hold once the change is
// archived, in OpenSpec's order: renames, removals, modifications,
// additions. Problems a maintainer must fix (a MODIFIED or REMOVED
// requirement that does not exist, an ADDED one that already does) are
// returned as warnings, and the change is applied as far as it can be.
func (d *Delta) Apply(truth []translate.Statement) ([]translate.Statement, []string) {
	var warn []string
	out := append([]translate.Statement{}, truth...)
	index := func() map[string]int {
		m := map[string]int{}
		for i, s := range out {
			m[s.ID] = i
		}
		return m
	}
	at := index()
	olds := make([]string, 0, len(d.Renamed))
	for old := range d.Renamed {
		olds = append(olds, old)
	}
	sort.Strings(olds)
	for _, old := range olds {
		i, ok := at[old]
		if !ok {
			warn = append(warn, fmt.Sprintf("RENAMED %s: no such requirement", old))
			continue
		}
		newTitle := d.titles[old]
		out[i].ID = d.Renamed[old]
		if first, rest, ok := strings.Cut(out[i].Prose, "\n"); ok && strings.HasPrefix(first, "Requirement: ") {
			out[i].Prose = "Requirement: " + newTitle + "\n" + rest
		} else {
			out[i].Prose = "Requirement: " + newTitle
		}
	}
	at = index()
	gone := map[int]bool{}
	for _, id := range d.Removed {
		i, ok := at[id]
		if !ok {
			warn = append(warn, fmt.Sprintf("REMOVED %s: no such requirement", id))
			continue
		}
		gone[i] = true
	}
	for _, s := range d.Modified {
		i, ok := at[s.ID]
		if !ok {
			warn = append(warn, fmt.Sprintf("MODIFIED %s: no such requirement; treated as added", s.ID))
			out = append(out, s)
			continue
		}
		out[i].Prose, out[i].Source, out[i].Introduced = s.Prose, s.Source, ""
	}
	for _, s := range d.Added {
		if _, ok := at[s.ID]; ok {
			warn = append(warn, fmt.Sprintf("ADDED %s: the requirement already exists; treated as modified", s.ID))
			i := at[s.ID]
			out[i].Prose, out[i].Source, out[i].Introduced = s.Prose, s.Source, ""
			continue
		}
		out = append(out, s)
	}
	var kept []translate.Statement
	for i, s := range out {
		if !gone[i] {
			kept = append(kept, s)
		}
	}
	return kept, warn
}
