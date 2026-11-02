// Package importer extracts normative statements from spec formats
// (docs/07 translator step 1, "segment"). Extraction is deterministic: no
// model decides what counts as a requirement.
package importer

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/translate"
)

var (
	frStart   = regexp.MustCompile(`^\[?(FR-[0-9A-Za-z.]+)\]?\s*[:.)-]?\s*`)
	normative = regexp.MustCompile(`\b(MUST|SHALL|MUST NOT|SHALL NOT|REQUIRED)\b`)
	folderNum = regexp.MustCompile(`^(\d+)-`)
)

// SpecKit reads a GitHub Spec Kit repository: every `- **FR-xxx**:` item in
// specs/*/spec.md, and every other list item, paragraph or table row in
// specs/*/*.md that uses an RFC 2119 keyword in capitals. Wrapped lines are
// one statement, and a list introduced by a line ending in ":" keeps that
// line (see normativeText). Statement IDs are `<folder number>/<FR id>` or
// `<folder number>/<file>:L<line>`; sources are `specs/<folder>/<file>#L<line>`.
func SpecKit(repo string, exclude ...string) ([]translate.Statement, error) {
	files, err := filepath.Glob(filepath.Join(repo, "specs", "*", "*.md"))
	if err != nil {
		return nil, err
	}
	kept := files[:0]
	for _, f := range files {
		if !excluded(f, exclude) {
			kept = append(kept, f)
		}
	}
	files = kept
	if len(files) == 0 {
		return nil, fmt.Errorf("no specs/*/*.md under %s (not a Spec Kit repository?): %w", repo, ErrNoSpecs)
	}
	sort.Strings(files)
	var out []translate.Statement
	seen := map[string]bool{}
	for _, f := range files {
		folder := filepath.Base(filepath.Dir(f))
		num := folder
		if m := folderNum.FindStringSubmatch(folder); m != nil {
			num = m[1]
		}
		base := filepath.Base(f)
		rel := filepath.ToSlash(filepath.Join("specs", folder, base))
		blocks, err := markdownBlocks(f)
		if err != nil {
			return nil, err
		}
		texts := normativeText(blocks, func(t string) bool {
			return normative.MatchString(t) || (base == "spec.md" && frStart.MatchString(t))
		})
		for i, b := range blocks {
			text := texts[i]
			if text == "" {
				continue
			}
			id := fmt.Sprintf("%s/%s:L%d", num, base, b.line)
			if m := frStart.FindStringSubmatch(b.text); m != nil && base == "spec.md" {
				id = num + "/" + m[1]
				if strings.HasPrefix(text, b.text) {
					text = m[1] + ": " + strings.TrimSpace(b.text[len(m[0]):]) + text[len(b.text):]
				}
			}
			if seen[id] { // a repeated FR id within one spec
				id = fmt.Sprintf("%s:L%d", id, b.line)
			}
			seen[id] = true
			out = append(out, translate.Statement{ID: id, Source: fmt.Sprintf("%s#L%d", rel, b.line), Prose: text})
		}
	}
	return out, nil
}

// NotePrefix starts the line Stele writes beside a superseded or retired
// statement (docs/08 W5 step 8). Importers skip it, so the note changes
// neither the statement's text nor its hash.
const NotePrefix = "> Stele "

// IsNote reports whether line is a note Stele wrote back.
func IsNote(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), NotePrefix) }

// clean strips markdown list and emphasis markers from a line.
func clean(line string) string {
	s := strings.TrimSpace(line)
	s = strings.TrimLeft(s, "-*> ")
	s = strings.TrimPrefix(s, "[ ] ")
	s = strings.TrimPrefix(s, "[x] ")
	s = strings.ReplaceAll(s, "**", "")
	return strings.TrimSpace(s)
}
