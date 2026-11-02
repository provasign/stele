package adjudicate

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Excerpt is a run of lines from one file of the repository.
type Excerpt struct {
	File  string   `json:"file"`
	From  int      `json:"from"` // first line, 1-based
	Lines []string `json:"lines"`
	Terms []string `json:"terms"` // search terms found in it
	Test  bool     `json:"test,omitempty"`
}

// Evidence is what a search of the repository found for a set of terms.
type Evidence struct {
	Excerpts []Excerpt      `json:"excerpts"`
	Hits     map[string]int `json:"hits"` // matching lines per term outside the specs; 0 means absent
}

// EvidenceOptions bound a search.
type EvidenceOptions struct {
	Exclude      []string // path prefixes not searched (the spec directories)
	MaxChars     int      // total size of the excerpts (default 30000)
	Context      int      // lines shown around a hit (default 4)
	FilesPerTerm int      // files shown per term (default 3)
	HitsPerFile  int      // hits shown per file (default 2)
	// NoDefinitions turns off showing where a searched name is defined.
	NoDefinitions bool
	// Define, when set, finds where a name is defined from a code graph
	// (D-062): whole bodies, by symbol, instead of text probes.
	Define func(name string) []LineRange
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*((\.|::)[A-Za-z_][A-Za-z0-9_]*)*$`)

// definedName is the name whose definition a term asks for: the last
// segment of an identifier-like term (Pkg.Func, mod::func), at least four
// characters, not all lower case without an underscore (a plain word).
func definedName(term string) string {
	if !identifier.MatchString(term) {
		return ""
	}
	name := term
	if i := strings.LastIndexAny(name, ".:"); i >= 0 {
		name = name[i+1:]
	}
	if len(name) < 4 || (strings.ToLower(name) == name && !strings.Contains(name, "_")) {
		return ""
	}
	return name
}

// definitionProbes are the ways common languages define a function, method
// or type named name.
func definitionProbes(name string) []string {
	return []string{
		"func " + name + "(", ") " + name + "(", // Go function, method
		"fn " + name + "(", "fn " + name + "<", // Rust
		"def " + name + "(",                     // Python, Ruby
		"fun " + name + "(",                     // Kotlin
		"function " + name + "(", name + " = (", // JavaScript, TypeScript
		"type " + name + " ", "struct " + name, "class " + name, "interface " + name, "enum " + name,
	}
}

var noise = []string{"node_modules/", "vendor/", "dist/", "build/", "target/", ".git/"}

// Gather searches the tracked files of repo for each term (git grep: fixed
// strings, case-sensitive unless the term has a space) and returns
// excerpts around the hits, tests first, then source, then other files,
// within opt.MaxChars.
func Gather(repo string, terms []string, opt EvidenceOptions) (*Evidence, error) {
	if opt.MaxChars == 0 {
		opt.MaxChars = 30000
	}
	if opt.Context == 0 {
		opt.Context = 4
	}
	if opt.FilesPerTerm == 0 {
		opt.FilesPerTerm = 3
	}
	if opt.HitsPerFile == 0 {
		opt.HitsPerFile = 2
	}
	ev := &Evidence{Hits: map[string]int{}}
	type hit struct {
		file string
		line int
	}
	// windows[file] collects [from, to] spans and the terms behind them.
	type span struct {
		from, to int
		terms    []string
	}
	windows := map[string][]span{}
	rank := map[string]int{}
	order := 0
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if len(term) < 3 {
			continue
		}
		hits, err := grep(repo, term, opt.Exclude)
		if err != nil {
			return nil, err
		}
		ev.Hits[term] = len(hits)
		files := map[string][]int{}
		var names []string
		for _, h := range hits {
			if _, ok := files[h.file]; !ok {
				names = append(names, h.file)
			}
			files[h.file] = append(files[h.file], h.line)
		}
		sort.SliceStable(names, func(i, j int) bool { return fileRank(names[i]) < fileRank(names[j]) })
		for _, f := range names[:min(len(names), opt.FilesPerTerm)] {
			lines := files[f]
			for _, n := range lines[:min(len(lines), opt.HitsPerFile)] {
				windows[f] = append(windows[f], span{max(1, n-opt.Context), n + opt.Context, []string{term}})
			}
			if _, ok := rank[f]; !ok {
				rank[f] = order
				order++
			}
		}
		// A call site rarely decides a question; the called function's body
		// does. For a name, show where it is defined, with its first lines.
		if name := definedName(term); name != "" && !opt.NoDefinitions {
			if opt.Define != nil {
				for _, d := range opt.Define(name) {
					if fileRank(d.File) == 0 || excluded(d.File, opt.Exclude) {
						continue
					}
					windows[d.File] = append(windows[d.File], span{d.From, min(d.To, d.From+60), []string{"definition of " + name}})
					rank[d.File] = -1 - order
					order++
				}
				continue
			}
			shown := 0
			for _, probe := range definitionProbes(name) {
				defs, err := grep(repo, probe, opt.Exclude)
				if err != nil {
					return nil, err
				}
				for _, h := range defs {
					if shown == 2 || fileRank(h.file) == 0 {
						continue
					}
					shown++
					windows[h.file] = append(windows[h.file], span{max(1, h.line-1), h.line + 30, []string{"definition of " + name}})
					rank[h.file] = -1 - order // definitions before other source
					order++
				}
				if shown == 2 {
					break
				}
			}
		}
	}
	var files []string
	for f := range windows {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool {
		if a, b := fileRank(files[i]), fileRank(files[j]); a != b {
			return a < b
		}
		return rank[files[i]] < rank[files[j]]
	})
	used := 0
	for _, f := range files {
		src, err := os.ReadFile(filepath.Join(repo, f))
		if err != nil {
			continue
		}
		all := strings.Split(string(src), "\n")
		spans := windows[f]
		sort.Slice(spans, func(i, j int) bool { return spans[i].from < spans[j].from })
		var merged []span
		for _, s := range spans {
			if k := len(merged) - 1; k >= 0 && s.from <= merged[k].to+1 {
				merged[k].to = max(merged[k].to, s.to)
				merged[k].terms = append(merged[k].terms, s.terms...)
				continue
			}
			merged = append(merged, s)
		}
		for _, s := range merged {
			to := min(s.to, len(all))
			if s.from > to {
				continue
			}
			lines := all[s.from-1 : to]
			size := 0
			for _, l := range lines {
				size += len(l) + 8
			}
			if used+size > opt.MaxChars {
				continue
			}
			used += size
			ev.Excerpts = append(ev.Excerpts, Excerpt{File: f, From: s.from, Lines: lines, Terms: dedupe(s.terms), Test: fileRank(f) == 0})
		}
	}
	return ev, nil
}

// Render prints excerpts with line numbers, as the model sees them.
func (ev *Evidence) Render() string {
	var b strings.Builder
	var absent []string
	for t, n := range ev.Hits {
		if n == 0 {
			absent = append(absent, t)
		}
	}
	sort.Strings(absent)
	if len(absent) > 0 {
		fmt.Fprintf(&b, "Terms found nowhere in the code: %s\n\n", strings.Join(absent, ", "))
	}
	for _, e := range ev.Excerpts {
		kind := ""
		if e.Test {
			kind = " (test)"
		}
		fmt.Fprintf(&b, "--- %s%s, lines %d-%d (terms: %s)\n", e.File, kind, e.From, e.From+len(e.Lines)-1, strings.Join(e.Terms, ", "))
		for i, l := range e.Lines {
			if len(l) > 300 {
				l = l[:300] + "…"
			}
			fmt.Fprintf(&b, "%6d  %s\n", e.From+i, l)
		}
		b.WriteString("\n")
	}
	return b.String()
}

type grepHit struct {
	file string
	line int
}

func grep(repo, term string, exclude []string) ([]grepHit, error) {
	args := []string{"-C", repo, "grep", "-n", "-I", "--no-color", "-F"}
	if strings.Contains(term, " ") {
		args = append(args, "-i")
	}
	args = append(args, "-e", term, "--", ".")
	for _, x := range append(append([]string{}, exclude...), noise...) {
		x = strings.TrimSuffix(x, "/")
		if x != "" {
			args = append(args, ":(exclude)"+x)
		}
	}
	cmd := exec.Command("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	src, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil // no match
		}
		return nil, fmt.Errorf("git grep %q: %v: %s", term, err, strings.TrimSpace(stderr.String()))
	}
	var out []grepHit
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		f, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		num, _, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		out = append(out, grepHit{f, n})
	}
	return out, sc.Err()
}

var testPath = regexp.MustCompile(`(^|/)(tests?|__tests__|testdata|e2e)/|_test\.\w+$|\.test\.\w+$|\.spec\.[jt]sx?$|(^|/)test_[^/]+\.py$|_tests?\.rs$`)

var sourceExt = map[string]bool{".go": true, ".rs": true, ".py": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".java": true, ".kt": true, ".cs": true, ".rb": true, ".php": true, ".swift": true, ".c": true, ".h": true, ".cc": true,
	".cpp": true, ".hpp": true, ".s": true, ".S": true, ".sh": true, ".zig": true, ".scala": true, ".ex": true, ".exs": true}

// fileRank orders evidence: tests 0, source 1, everything else 2.
func fileRank(f string) int {
	if testPath.MatchString(f) {
		return 0
	}
	if sourceExt[filepath.Ext(f)] {
		return 1
	}
	return 2
}

func excluded(f string, exclude []string) bool {
	for _, x := range append(append([]string{}, exclude...), noise...) {
		x = strings.TrimSuffix(x, "/")
		if x != "" && (f == x || strings.HasPrefix(f, x+"/") || strings.Contains(f, "/"+x+"/")) {
			return true
		}
	}
	return false
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// SpecDirs are the top directories holding the statements' sources, which
// a code search leaves out.
func SpecDirs(sources []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sources {
		file, _, _ := strings.Cut(s, "#")
		dir := file
		if i := strings.Index(file, "/"); i >= 0 {
			dir = file[:i]
		}
		if dir != "" && !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out
}

var backticked = regexp.MustCompile("`([^`\n]{3,80})`")

// QuotedNames are the backticked spans of a statement's text: names the
// author wrote as code.
func QuotedNames(prose string) []string {
	var out []string
	for _, m := range backticked.FindAllStringSubmatch(prose, -1) {
		out = append(out, m[1])
	}
	return dedupe(out)
}
