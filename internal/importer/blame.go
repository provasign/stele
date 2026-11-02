package importer

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// blame returns the commit date (YYYY-MM-DD, UTC) of every line of file, a
// path relative to the git repository repo.
func blame(repo, file string) (map[int]string, error) {
	cmd := exec.Command("git", "-C", repo, "blame", "--line-porcelain", "--", file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	src, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git blame %s: %v: %s", file, err, strings.TrimSpace(stderr.String()))
	}
	out := map[int]string{}
	line := 0
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "\t") { // the line's content
			continue
		}
		f := strings.Fields(sc.Text())
		switch {
		case len(f) >= 3 && len(f[0]) == 40 && isHex(f[0]):
			line, _ = strconv.Atoi(f[2]) // <sha> <orig line> <final line> [count]
		case len(f) == 2 && f[0] == "committer-time":
			if sec, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				out[line] = time.Unix(sec, 0).UTC().Format(time.RFC3339) // to the second: statements from different commits of one day still order (ISSUES 34)
			}
		}
	}
	return out, sc.Err()
}

// newest is the latest date among lines first..last; empty when none is known.
func newest(when map[int]string, first, last int) string {
	best := ""
	for n := first; n <= last; n++ {
		if d := when[n]; d > best {
			best = d
		}
	}
	return best
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
