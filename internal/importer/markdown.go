package importer

import (
	"bufio"
	"errors"
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
	heading   = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	listItem  = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])\s+(.*)$`)
	explicit  = regexp.MustCompile(`^\**\s*\[?([A-Z][A-Z0-9]*-[0-9][0-9A-Za-z.\-]*)\]?\**\s*[:.)-]?\s*`)
	normLower = regexp.MustCompile(`(?i)\b(must|shall|required|should)\b`)
	// MAY is RFC 2119's too: "MAY borrow only when the tool is available"
	// and "Only a steward MAY mark it" restrict, and a plain MAY is a
	// permission Stele checks can happen. Left out, a trial's specs lost
	// three of eight requirements with no word (2026-11-11). Lowercase
	// "may" stays out: in prose it is rarely a requirement.
	normUpper = regexp.MustCompile(`\b(MUST|SHALL|REQUIRED|SHOULD|MAY)\b`)
	slugDrop  = regexp.MustCompile(`[^a-z0-9]+`)
)

// MarkdownOptions control the plain-markdown importer.
type MarkdownOptions struct {
	// AnyCase also takes lower-case must, shall, should and required, for
	// specs that do not write RFC 2119 keywords in capitals.
	AnyCase bool
	Exclude []string // generated registry/state paths, never spec input
}

// Markdown reads every .md file under dir, for specs in no particular
// format. A statement is a list item or paragraph outside code blocks that
// starts with a requirement ID (FR-001, REQ-12) or uses an RFC 2119 keyword
// (MUST, SHALL, SHOULD, REQUIRED; with AnyCase, in any case). Its prose starts with the section it is in. Its ID is an
// explicit one at the start of the text (FR-001, REQ-12) when there is one,
// else `<file>#<section>/<n>`, the n-th statement of that section, so
// lines added elsewhere in the file do not change it. Sources are
// `<file>#L<line>`.
func Markdown(dir string, opt MarkdownOptions) ([]translate.Statement, error) {
	var files []string
	// A folder that is not there yet holds no requirement: a new project's
	// first check read "lstat requirements: no such file or directory".
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no folder %s: %w", dir, ErrNoSpecs)
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if excluded(p, opt.Exclude) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != dir && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "target" || name == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(name), ".md") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .md files under %s: %w", dir, ErrNoSpecs)
	}
	sort.Strings(files)
	norm := normUpper
	if opt.AnyCase {
		norm = normLower
	}
	var out []translate.Statement
	seen := map[string]bool{}
	for _, f := range files {
		rel, _ := filepath.Rel(dir, f)
		rel = filepath.ToSlash(rel)
		blocks, err := markdownBlocks(f)
		if err != nil {
			return nil, err
		}
		perSection := map[string]int{}
		texts := normativeText(blocks, func(t string) bool { return explicit.MatchString(t) || norm.MatchString(t) })
		for i, b := range blocks {
			text := texts[i]
			if text == "" {
				continue
			}
			m := explicit.FindStringSubmatch(b.text)
			sec := slug(b.section)
			perSection[sec]++
			id := fmt.Sprintf("%s#%s/%d", rel, sec, perSection[sec])
			if m != nil {
				id = rel + "#" + m[1]
			}
			for seen[id] {
				id += "'"
			}
			seen[id] = true
			prose := text
			if b.section != "" {
				prose = "Section: " + b.section + "\n" + text
			}
			out = append(out, translate.Statement{ID: id, Source: fmt.Sprintf("%s#L%d", rel, b.line), Prose: prose})
		}
	}
	return out, nil
}

type mdBlock struct {
	section string // heading path, outermost first
	text    string
	line    int
	item    bool // a list item (or table row), not a paragraph
	indent  int  // a list item's indentation
}

// markdownBlocks splits a file into list items, paragraphs and table rows,
// with the heading path above each. Code blocks, HTML comments and Stele's
// own notes are skipped. A table row is one block, each cell labelled with
// its column header ("FR-004: Trigger: WHEN ...; Response: ...").
func markdownBlocks(path string) ([]mdBlock, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var out []mdBlock
	var headings []string
	var cur []string
	var header []string // the current table's column headers
	start, indent, item := 0, 0, false
	flush := func() {
		if len(cur) > 0 {
			out = append(out, mdBlock{section: strings.Join(headings, " > "), text: strings.Join(cur, " "), line: start, item: item, indent: indent})
		}
		cur = nil
	}
	fence, comment := false, false
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~"):
			flush()
			fence = !fence
			continue
		case fence:
			continue
		case comment:
			comment = !strings.Contains(trim, "-->")
			continue
		case strings.HasPrefix(trim, "<!--"):
			flush()
			comment = !strings.Contains(trim, "-->")
			continue
		case IsNote(line):
			continue
		}
		if !strings.HasPrefix(trim, "|") {
			header = nil
		}
		if m := heading.FindStringSubmatch(trim); m != nil && !strings.HasPrefix(line, " ") {
			flush()
			level := len(m[1])
			if len(headings) >= level {
				headings = headings[:level-1]
			}
			for len(headings) < level-1 {
				headings = append(headings, "")
			}
			headings = append(headings, strings.ReplaceAll(m[2], "**", ""))
			continue
		}
		switch {
		case strings.HasPrefix(trim, "|"):
			flush()
			cells := tableCells(trim)
			switch {
			case tableRule.MatchString(trim):
			case header == nil:
				header = cells
			default:
				start, indent, item = n, 0, true
				cur = append(cur, tableRow(header, cells))
				flush()
			}
		case trim == "" || strings.HasPrefix(trim, "---"):
			flush()
		case listItem.MatchString(line):
			flush()
			m := listItem.FindStringSubmatch(line)
			start, indent, item = n, len(strings.ReplaceAll(m[1], "\t", "    ")), true
			cur = append(cur, clean(m[3]))
		default:
			if len(cur) == 0 {
				start, indent, item = n, 0, false
			}
			cur = append(cur, clean(trim))
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i := range out {
		out[i].section = strings.Trim(strings.ReplaceAll(out[i].section, " >  > ", " > "), " >")
	}
	return out, nil
}

var tableRule = regexp.MustCompile(`^\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?$`)

func tableCells(row string) []string {
	row = strings.TrimSuffix(strings.TrimPrefix(row, "|"), "|")
	var cells []string
	for _, c := range strings.Split(row, "|") {
		cells = append(cells, clean(c))
	}
	return cells
}

// tableRow renders a table row as text: a leading requirement ID stays in
// front ("FR-004: ..."), and every other cell is labelled with its header.
func tableRow(header, cells []string) string {
	var parts []string
	lead := ""
	for i, c := range cells {
		if c == "" {
			continue
		}
		if i == 0 && explicitOnly.MatchString(c) {
			lead = c + ": "
			continue
		}
		if i < len(header) && header[i] != "" {
			c = header[i] + ": " + c
		}
		parts = append(parts, c)
	}
	return lead + strings.Join(parts, "; ")
}

var explicitOnly = regexp.MustCompile(`^\[?[A-Z][A-Z0-9]*-[0-9][0-9A-Za-z.\-]*\]?$`)

// leadIn is a block ending in a colon that introduces the list items after
// it ("Names MUST NOT:", "- FR-018: The system MUST:"), with those items.
type leadIn struct {
	lead     int
	children []int
}

// leadIns finds the lead-ins among blocks: each list item that follows a
// block ending in ":" in the same section, before the next paragraph or
// item with a requirement ID, and (when the lead is itself an item) nested
// under it.
func leadIns(blocks []mdBlock) (map[int]*leadIn, map[int]*leadIn) {
	byLead, byChild := map[int]*leadIn{}, map[int]*leadIn{}
	for i := 0; i < len(blocks); i++ {
		b := blocks[i]
		if !strings.HasSuffix(b.text, ":") {
			continue
		}
		l := &leadIn{lead: i}
		for j := i + 1; j < len(blocks); j++ {
			c := blocks[j]
			if !c.item || c.section != b.section || (b.item && c.indent <= b.indent) || byChild[j] != nil ||
				(len(l.children) > 0 && c.indent < blocks[l.children[0]].indent) || explicit.MatchString(c.text) {
				break // a new requirement ("FR-012: ...") starts its own list
			}
			l.children = append(l.children, j)
		}
		if len(l.children) == 0 {
			continue
		}
		byLead[i] = l
		for _, j := range l.children {
			byChild[j] = l
		}
	}
	return byLead, byChild
}

// normativeText gives each block's text as a statement, or "" when it is
// not one. A lead-in carries its items: a normative lead whose items are
// not normative is one statement holding them all ("MUST be wired at: the
// CLI; the TUI"); a normative item under a lead starts with the lead, so
// "It MUST NOT begin with -" keeps its subject. A lead whose items speak
// for themselves is not a statement of its own.
func normativeText(blocks []mdBlock, isStatement func(string) bool) []string {
	out := make([]string, len(blocks))
	byLead, byChild := leadIns(blocks)
	for i, b := range blocks {
		if l := byLead[i]; l != nil {
			any := false
			for _, j := range l.children {
				if isStatement(blocks[j].text) {
					any = true
				}
			}
			if !any && isStatement(b.text) {
				var items []string
				for _, j := range l.children {
					items = append(items, strings.TrimSuffix(blocks[j].text, ";"))
				}
				out[i] = b.text + " " + strings.Join(items, "; ")
			}
			if any || isStatement(b.text) || byChild[i] == nil {
				continue
			}
		}
		if l := byChild[i]; l != nil {
			if !isStatement(b.text) {
				continue
			}
			lead := blocks[l.lead].text
			if explicit.MatchString(b.text) && !isStatement(lead) {
				out[i] = b.text
			} else {
				out[i] = lead + " " + b.text
			}
			continue
		}
		if isStatement(b.text) {
			out[i] = b.text
		}
	}
	return out
}

func slug(heading string) string {
	if strings.TrimSpace(heading) == "" {
		return "top"
	}
	s := strings.Trim(slugDrop.ReplaceAllString(strings.ToLower(heading), "-"), "-")
	if len(s) > 60 {
		s = strings.TrimRight(s[:60], "-")
	}
	for _, r := range heading {
		if r > 127 { // letters the slug drops: keep sections apart by a hash
			return strings.Trim(s+"-h"+translate.TextHash(heading)[:6], "-")
		}
	}
	return s
}
