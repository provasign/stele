package adopt

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/translate"
)

// Note is one line written beside a losing statement.
type Note struct {
	File string
	Line int // the statement's line; the note goes on the line after it
	Text string
}

// WriteBack adds a note beside each statement an accepted or reversed item
// supersedes (docs/08 W5 step 8), so agents reading the spec see that it no
// longer holds where the two conflict. Importers skip these notes, so the
// statement keeps its text hash. Notes already present are left alone. With
// dryRun nothing is written.
func WriteBack(repo string, items []Item, stmts []translate.Statement, dryRun bool) ([]Note, error) {
	byID := map[string]translate.Statement{}
	for _, s := range stmts {
		byID[s.ID] = s
	}
	byFile := map[string][]Note{}
	for _, it := range items {
		if it.Decision != Accept && it.Decision != Reverse {
			continue
		}
		ref := "triage " + it.ID
		if (it.Decision == Accept && len(it.Retire) > 0) || (it.Decision == Reverse && len(it.RetireIfReversed) > 0) {
			ref = "decision D-" + it.ID
		}
		winners := map[string][]string{} // loser -> winners
		for _, p := range it.Pairs() {
			winners[p[1]] = appendNew(winners[p[1]], p[0])
		}
		for loser, ws := range winners {
			s, ok := byID[loser]
			if !ok {
				return nil, fmt.Errorf("%s: statement %s is not in the statements file", it.ID, loser)
			}
			file, line, err := splitSource(s.Source)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", it.ID, err)
			}
			text := fmt.Sprintf("%s%s: superseded by %s where they conflict (%s): %s", importer.NotePrefix, it.ID, strings.Join(ws, " and "), ref, oneLine(it.Situation))
			byFile[file] = append(byFile[file], Note{File: file, Line: line, Text: text})
		}
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	var written []Note
	for _, f := range files {
		notes := byFile[f]
		path := filepath.Join(repo, filepath.FromSlash(f))
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(src), "\n")
		// Bottom up, so earlier line numbers stay valid.
		sort.SliceStable(notes, func(i, j int) bool {
			if notes[i].Line != notes[j].Line {
				return notes[i].Line > notes[j].Line
			}
			return notes[i].Text > notes[j].Text
		})
		for _, n := range notes {
			if n.Line < 1 || n.Line > len(lines) {
				return nil, fmt.Errorf("%s#L%d: past the end of the file; re-import first", f, n.Line)
			}
			tag := strings.SplitN(strings.TrimPrefix(n.Text, importer.NotePrefix), ":", 2)[0] + ":"
			present := false
			for k := n.Line; k < len(lines) && importer.IsNote(lines[k]); k++ {
				present = present || strings.Contains(lines[k], importer.NotePrefix+tag)
			}
			if present {
				continue
			}
			n.Text = indentOf(lines[n.Line-1]) + n.Text
			lines = append(lines[:n.Line], append([]string{n.Text}, lines[n.Line:]...)...)
			written = append(written, n)
		}
		if !dryRun {
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
				return nil, err
			}
		}
	}
	return written, nil
}

func splitSource(src string) (string, int, error) {
	file, l, ok := strings.Cut(src, "#L")
	n, err := strconv.Atoi(l)
	if !ok || err != nil {
		return "", 0, fmt.Errorf("source %q has no #L<line>", src)
	}
	return file, n, nil
}

// indentOf nests the note under a list item, or matches the line's indent.
func indentOf(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	indent := line[:len(line)-len(trimmed)]
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
		indent += "  "
	}
	return indent
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 240 {
		s = string(r[:237]) + "..."
	}
	return s
}
