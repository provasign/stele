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

var (
	requirementHead = regexp.MustCompile(`^###\s+Requirement:\s*(.+?)\s*$`)
	sectionHead     = regexp.MustCompile(`^#{1,3}\s`) // ends a requirement; scenarios are ####
)

// OpenSpec reads an OpenSpec repository: every `### Requirement:` block in
// openspec/specs/**/spec.md, with its description and scenarios, is one
// statement. Change folders left inside the specs tree are read too, because
// agents read them. Statement IDs are `<capability>/<title>`, with `:L<line>`
// on every copy of a title stated more than once in a file; sources are
// `openspec/specs/<capability>/spec.md#L<line>` of the requirement heading.
// With dates, each statement's Introduced is the newest commit time among
// the block's lines (git blame, UTC, RFC 3339): when its current text was
// written.
func OpenSpec(repo string, dates bool, exclude ...string) ([]translate.Statement, error) {
	root := filepath.Join(repo, "openspec", "specs")
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if excluded(p, exclude) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && d.Name() == "spec.md" {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w (not an OpenSpec repository?)", root, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no openspec/specs/**/spec.md under %s (not an OpenSpec repository?): %w", repo, ErrNoSpecs)
	}
	sort.Strings(files)
	var out []translate.Statement
	for _, f := range files {
		rel, err := filepath.Rel(repo, f)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		capability := strings.TrimSuffix(strings.TrimPrefix(rel, "openspec/specs/"), "/spec.md")
		reqs, err := requirements(f)
		if err != nil {
			return nil, err
		}
		var when map[int]string
		if dates {
			if when, err = blame(repo, rel); err != nil {
				return nil, err
			}
		}
		count := map[string]int{}
		for _, r := range reqs {
			count[r.title]++
		}
		for _, r := range reqs {
			id := capability + "/" + r.title
			if count[r.title] > 1 {
				id = fmt.Sprintf("%s:L%d", id, r.line)
			}
			out = append(out, translate.Statement{
				ID:         id,
				Source:     fmt.Sprintf("%s#L%d", rel, r.line),
				Prose:      r.text,
				Introduced: newest(when, r.line, r.end),
			})
		}
	}
	return out, nil
}

type requirement struct {
	title     string
	line, end int // heading line and last line, 1-based
	text      string
}

// requirements splits one spec file into its requirement blocks.
func requirements(path string) ([]requirement, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var out []requirement
	var cur *requirement
	var body []string
	flush := func(last int) {
		if cur == nil {
			return
		}
		for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
			body = body[:len(body)-1]
			last--
		}
		cur.end = last
		cur.text = "Requirement: " + cur.title
		if b := strings.TrimSpace(strings.Join(body, "\n")); b != "" {
			cur.text += "\n" + strings.ReplaceAll(b, "**", "")
		}
		out = append(out, *cur)
		cur, body = nil, nil
	}
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if m := requirementHead.FindStringSubmatch(line); m != nil {
			flush(n - 1)
			cur = &requirement{title: m[1], line: n}
			continue
		}
		if sectionHead.MatchString(line) {
			flush(n - 1)
			continue
		}
		if cur != nil && !IsNote(line) {
			body = append(body, line)
		}
	}
	flush(n)
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}
