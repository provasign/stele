// Package adjudicate decides suspected conflicts between spec statements
// from the project's code: it dates each statement from git, gathers the
// code and tests that mention what the statements are about, and asks one
// model call which statements the code follows. See D-055.
package adjudicate

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/provasign/stele/internal/translate"
)

// Dated is when a statement's current text was last written, and by which
// commit.
type Dated struct {
	Date    string `json:"date,omitempty"`    // YYYY-MM-DD, UTC
	Commit  string `json:"commit,omitempty"`  // abbreviated hash
	Subject string `json:"subject,omitempty"` // the commit's subject line
}

// DateStatements dates every statement whose source is "<file>#L<line>" in
// repo. A statement's text runs from its line to the line before the next
// statement in the same file (at most 200 lines); its date is that of the
// commit that wrote most of those lines (ties go to the newer), so a later
// one-line touch-up does not make an old requirement look new. A statement
// whose file git cannot blame keeps its Introduced date, if any.
func DateStatements(repo string, stmts []translate.Statement) map[string]Dated {
	type at struct {
		id   string
		line int
	}
	byFile := map[string][]at{}
	out := map[string]Dated{}
	for _, s := range stmts {
		if s.Introduced != "" {
			out[s.ID] = Dated{Date: s.Introduced}
		}
		file, line, ok := splitSource(s.Source)
		if !ok {
			continue
		}
		byFile[file] = append(byFile[file], at{s.ID, line})
	}
	for file, list := range byFile {
		lines, err := blameLines(repo, file)
		if err != nil {
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].line < list[j].line })
		for i, a := range list {
			last := a.line + 199
			if i+1 < len(list) && list[i+1].line-1 < last {
				last = max(a.line, list[i+1].line-1)
			}
			count := map[string]int{}
			commits := map[string]blamed{}
			for n := a.line; n <= last; n++ {
				if b, ok := lines[n]; ok && b.unix > 0 {
					count[b.sha]++
					commits[b.sha] = b
				}
			}
			var best blamed
			for sha, n := range count {
				b := commits[sha]
				if n > count[best.sha] || (n == count[best.sha] && b.unix > best.unix) {
					best = b
				}
			}
			if best.unix > 0 {
				out[a.id] = Dated{Date: time.Unix(best.unix, 0).UTC().Format("2006-01-02"), Commit: best.sha[:min(10, len(best.sha))], Subject: best.subject}
			}
		}
	}
	return out
}

// splitSource reads "path#L12" (also "path#L12-L20").
func splitSource(src string) (string, int, bool) {
	i := strings.LastIndex(src, "#L")
	if i < 0 {
		return "", 0, false
	}
	num := src[i+2:]
	if j := strings.IndexAny(num, "-:"); j >= 0 {
		num = num[:j]
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 {
		return "", 0, false
	}
	return src[:i], n, true
}

type blamed struct {
	sha     string
	unix    int64
	subject string
}

// blameLines returns, for each line of file, the commit that last wrote it.
func blameLines(repo, file string) (map[int]blamed, error) {
	cmd := exec.Command("git", "-C", repo, "blame", "--line-porcelain", "--", file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	src, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git blame %s: %v: %s", file, err, strings.TrimSpace(stderr.String()))
	}
	out := map[int]blamed{}
	var cur blamed
	line := 0
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		t := sc.Text()
		if strings.HasPrefix(t, "\t") { // the line's content ends its record
			out[line] = cur
			continue
		}
		f := strings.Fields(t)
		switch {
		case len(f) >= 3 && len(f[0]) == 40:
			cur = blamed{sha: f[0]}
			line, _ = strconv.Atoi(f[2])
		case len(f) == 2 && f[0] == "committer-time":
			cur.unix, _ = strconv.ParseInt(f[1], 10, 64)
		case strings.HasPrefix(t, "summary "):
			cur.subject = strings.TrimPrefix(t, "summary ")
		}
	}
	return out, sc.Err()
}
