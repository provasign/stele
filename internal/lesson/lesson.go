// Package lesson keeps what the owner taught Stele about a project (D-063):
// one file per lesson under the registry's lessons/ directory, committed
// with the specs, read by every later translation and coverage check. A
// lesson's ID is a hash of its content, so lessons added on parallel
// branches never share a file or an ID and merge without conflict.
package lesson

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/ir"
)

// Dir is the registry subdirectory holding lessons.
const Dir = "lessons"

// MaxProjectWide is how many project-wide lessons every statement gets
// before relevance decides: below it, all of them.
const MaxProjectWide = 30

// ID is the lesson's content hash: its kind, its words and its pins.
func ID(l ir.Lesson) string {
	var pins []string
	for s, h := range l.Statements {
		pins = append(pins, s+"="+h)
	}
	sort.Strings(pins)
	sum := sha256.Sum256([]byte(l.Kind + "\x00" + strings.Join(strings.Fields(l.Says), " ") + "\x00" + strings.Join(pins, "\x00")))
	return fmt.Sprintf("L-%x", sum[:3])
}

type doc struct {
	Lesson     string            `yaml:"lesson"`
	Kind       string            `yaml:"kind"`
	Says       string            `yaml:"says"`
	Statements map[string]string `yaml:"statements,omitempty"`
	Terms      []string          `yaml:"terms,omitempty"`
	From       string            `yaml:"from,omitempty"`
	By         string            `yaml:"by,omitempty"`
	At         string            `yaml:"at,omitempty"`
}

// Add writes the lesson to dir/lessons/<id>.yaml and returns its ID. The
// same lesson added again is not written twice (added is false).
func Add(dir string, l ir.Lesson) (id string, added bool, err error) {
	switch l.Kind {
	case ir.LessonTerm, ir.LessonFact, ir.LessonReading, ir.LessonScope:
	default:
		return "", false, fmt.Errorf("lesson kind must be term, fact, reading or scope, not %q", l.Kind)
	}
	if strings.TrimSpace(l.Says) == "" {
		return "", false, fmt.Errorf("a lesson says something: --says is empty")
	}
	if l.Kind == ir.LessonReading && !l.Pinned() {
		return "", false, fmt.Errorf("a reading lesson names the statement it is about")
	}
	id = ID(l)
	path := filepath.Join(dir, Dir, id+".yaml")
	if _, err := os.Stat(path); err == nil {
		return id, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", false, err
	}
	src, err := yaml.Marshal(doc{Lesson: id, Kind: l.Kind, Says: strings.TrimSpace(l.Says), Statements: l.Statements, Terms: l.Terms, From: l.From, By: l.By, At: l.At})
	if err != nil {
		return "", false, err
	}
	header := "# What the owner taught Stele about this project (D-063). Committed with the specs;\n# delete this file to remove the lesson.\n"
	return id, true, os.WriteFile(path, append([]byte(header), src...), 0o644)
}

// Remove deletes a lesson's file.
func Remove(dir, id string) error {
	path := filepath.Join(dir, Dir, id+".yaml")
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no lesson %s in %s", id, filepath.Join(dir, Dir))
	} else if err != nil {
		return err
	}
	return nil
}

// Relevant lists the lessons that apply to a statement as now written:
// those pinned to it, and the project-wide ones, all of them when there are
// at most MaxProjectWide, otherwise the max that share most words with it.
func Relevant(ls []*ir.Lesson, stmt, textHash, prose string, max int) []*ir.Lesson {
	var pinned, wide []*ir.Lesson
	for _, l := range ls {
		if !l.Pinned() {
			wide = append(wide, l)
			continue
		}
		if ok, _ := l.AppliesTo(stmt, textHash); ok {
			pinned = append(pinned, l)
		}
	}
	if len(wide) > MaxProjectWide {
		text := words(prose)
		type scored struct {
			l *ir.Lesson
			n int
		}
		var ss []scored
		for _, l := range wide {
			n := 0
			for w := range words(l.Says + " " + strings.Join(l.Terms, " ")) {
				if text[w] {
					n++
				}
			}
			if n > 0 {
				ss = append(ss, scored{l, n})
			}
		}
		sort.SliceStable(ss, func(i, j int) bool { return ss[i].n > ss[j].n })
		wide = wide[:0]
		for i := 0; i < len(ss) && i < max; i++ {
			wide = append(wide, ss[i].l)
		}
	}
	return append(pinned, wide...)
}

// Note is how a lesson reaches the translator.
func Note(l *ir.Lesson) string {
	return fmt.Sprintf("review: the owner taught Stele this about the project (%s): %s", l.ID, l.Says)
}

// Stale lists pinned lessons whose statement's text has changed since: they
// are kept, but no longer applied.
func Stale(ls []*ir.Lesson, current map[string]string) []*ir.Lesson {
	var out []*ir.Lesson
	for _, l := range ls {
		for s := range l.Statements {
			if h, ok := current[s]; ok {
				if _, stale := l.AppliesTo(s, h); stale {
					out = append(out, l)
					break
				}
			}
		}
	}
	return out
}

// Contradictions pairs lessons of one kind that concern the same terms and
// say different things: two branches can each teach something the other
// contradicts, and git merges them cleanly.
func Contradictions(ls []*ir.Lesson) [][2]*ir.Lesson {
	var out [][2]*ir.Lesson
	for i, a := range ls {
		for _, b := range ls[i+1:] {
			if a.Kind != b.Kind || a.Kind == ir.LessonReading || strings.EqualFold(strings.TrimSpace(a.Says), strings.TrimSpace(b.Says)) {
				continue
			}
			if shared(a.Terms, b.Terms) {
				out = append(out, [2]*ir.Lesson{a, b})
			}
		}
	}
	return out
}

func shared(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if strings.EqualFold(x, y) {
				return true
			}
		}
	}
	return false
}

// words are a text's lower-cased words of three letters or more, without
// the words every requirement uses.
func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) >= 3 && !common[w] {
			out[w] = true
		}
	}
	return out
}

var common = map[string]bool{"the": true, "and": true, "must": true, "shall": true, "should": true, "system": true, "when": true, "with": true,
	"for": true, "not": true, "that": true, "this": true, "are": true, "its": true, "from": true, "use": true, "same": true, "any": true}

// Load reads the lessons under dir/lessons, for commands that start from a
// registry directory without loading the whole registry. A missing
// directory has none.
func Load(dir string) ([]*ir.Lesson, error) {
	files, err := filepath.Glob(filepath.Join(dir, Dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []*ir.Lesson
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var d doc
		if err := yaml.Unmarshal(src, &d); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if d.Lesson == "" {
			continue
		}
		out = append(out, &ir.Lesson{ID: d.Lesson, Kind: d.Kind, Says: d.Says, Statements: d.Statements, Terms: d.Terms, From: d.From, By: d.By, At: d.At, File: f})
	}
	return out, nil
}

// Guidance reports whether a lesson is about how to read requirements, so
// the coverage judge should follow it too.
func Guidance(l *ir.Lesson) bool { return l.Kind == ir.LessonScope || l.Kind == ir.LessonReading }
