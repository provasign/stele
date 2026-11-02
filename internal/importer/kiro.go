package importer

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/translate"
)

var (
	kiroRequirement = regexp.MustCompile(`(?i)^requirement\s+([0-9][0-9.]*)\b\s*[:.-]?\s*(.*)$`)
	kiroNumber      = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)+)\.?\s+`)
	kiroNormative   = regexp.MustCompile(`(?i)\b(shall|must)\b`)
	kiroGlossary    = regexp.MustCompile(`^([A-Za-z][\w .-]*?)\s*:\s+(.+)$`)
	// Sections that describe things other than required behavior: the
	// defect a bugfix spec fixes, background, definitions and exclusions.
	kiroSkip = regexp.MustCompile(`(?i)\b(current behaviou?r|defect|introduction|overview|background|glossary|out of scope|non-goals?|root cause|evidence|findings|user story)\b`)
)

// Kiro reads a Kiro repository: the requirements.md and bugfix.md of every
// feature under .kiro/specs (design.md and tasks.md are not requirements).
// A statement is a normative acceptance criterion or other normative item:
// under "Requirement N", its ID is `<feature>/R<N>.<k>`, the k-th statement
// of that requirement; a bugfix spec's numbered criteria ("2.1 WHEN ...")
// keep their number, `<feature>/2.1`. Bug analysis that describes current,
// defective behavior is skipped. The prose starts with the requirement's
// title, and ends with the glossary entries for the terms it uses, so
// "the Build_Script" carries its meaning. Archived specs (a directory named
// archive or archived) are skipped. Sources are `<file>#L<line>`, relative
// to the repository root.
func Kiro(repo string, exclude ...string) ([]translate.Statement, error) {
	root := filepath.Join(repo, ".kiro", "specs")
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
		if d.IsDir() {
			if n := strings.ToLower(d.Name()); p != root && (n == "archive" || n == "archived" || strings.HasPrefix(n, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if n := strings.ToLower(d.Name()); n == "requirements.md" || n == "bugfix.md" {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w (not a Kiro repository?)", root, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .kiro/specs/*/requirements.md or bugfix.md under %s: %w", repo, ErrNoSpecs)
	}
	sort.Strings(files)
	var out []translate.Statement
	seen := map[string]bool{}
	for _, f := range files {
		rel, err := filepath.Rel(repo, f)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		feature, _ := filepath.Rel(root, filepath.Dir(f))
		feature = filepath.ToSlash(feature)
		blocks, err := markdownBlocks(f)
		if err != nil {
			return nil, err
		}
		glossary := map[string]string{}
		var kept []mdBlock
		for _, b := range blocks {
			if strings.Contains(strings.ToLower(b.section), "glossary") {
				if m := kiroGlossary.FindStringSubmatch(b.text); m != nil {
					glossary[strings.TrimSpace(m[1])] = strings.TrimSpace(m[2])
				}
				continue
			}
			if kiroSkip.MatchString(lastHeading(b.section)) || strings.HasPrefix(strings.ToLower(b.text), "user story") {
				continue
			}
			kept = append(kept, b)
		}
		terms := make([]string, 0, len(glossary))
		for t := range glossary {
			terms = append(terms, t)
		}
		sort.Strings(terms)
		texts := normativeText(kept, kiroNormative.MatchString)
		perReq := map[string]int{}
		for i, b := range kept {
			text := texts[i]
			if text == "" {
				continue
			}
			req, title := kiroRequirementOf(b.section)
			var id string
			if m := kiroNumber.FindStringSubmatch(b.text); m != nil && req == "" {
				id = feature + "/" + m[1]
			} else {
				key := "R" + req
				if req == "" {
					key = slug(lastHeading(b.section))
				}
				perReq[key]++
				id = fmt.Sprintf("%s/%s.%d", feature, key, perReq[key])
			}
			for seen[id] {
				id += "'"
			}
			seen[id] = true
			prose := text
			if title != "" {
				prose = title + "\n" + text
			}
			var defs []string
			for _, t := range terms {
				if regexp.MustCompile(`\b` + regexp.QuoteMeta(t) + `\b`).MatchString(text) {
					defs = append(defs, t+": "+glossary[t])
				}
			}
			if len(defs) > 0 {
				prose += "\nGlossary: " + strings.Join(defs, " | ")
			}
			out = append(out, translate.Statement{ID: id, Source: fmt.Sprintf("%s#L%d", rel, b.line), Prose: prose})
		}
	}
	return out, nil
}

// kiroRequirementOf finds the "Requirement N: title" heading in a section
// path, returning N and the heading.
func kiroRequirementOf(section string) (string, string) {
	parts := strings.Split(section, " > ")
	for i := len(parts) - 1; i >= 0; i-- {
		if m := kiroRequirement.FindStringSubmatch(strings.TrimSpace(parts[i])); m != nil {
			return strings.TrimSuffix(m[1], "."), strings.TrimSpace(parts[i])
		}
	}
	return "", ""
}

func lastHeading(section string) string {
	parts := strings.Split(section, " > ")
	return parts[len(parts)-1]
}
