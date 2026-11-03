package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/touch"
)

// cmdTouches prints the approved requirements the current code change
// touches: rules whose step or attributes name code the branch or the
// working tree adds, edits or removes. Like `stele rules`, it prints nothing
// when the change touches nothing, and a hook never blocks on it.
func cmdTouches(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("touches", flag.ContinueOnError)
	project := fs.String("project", ".", "directory whose stele.yaml names the registry")
	base := fs.String("base", "", "compare the working tree with this ref (default: where the branch left the base branch)")
	k := fs.Int("k", 8, "most statements to show")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Find(*project)
	if err != nil || cfg == nil {
		if err == nil {
			err = fmt.Errorf("no %s in %s or its parents", config.FileName, *project)
		}
		return failf("touches", err)
	}
	reg, err := ir.LoadDir(cfg.RegistryDir())
	if err != nil {
		return failf("touches", err)
	}
	if *base == "" {
		*base = forkPoint(*project)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	changed, err := touch.Changes(ctx, *project, *base)
	if err != nil {
		return failf("touches", err)
	}
	ts := touch.Rules(reg, changed)
	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if ts == nil {
			ts = []touch.Touched{}
		}
		_ = enc.Encode(map[string]any{"base": *base, "changed_symbols": len(changed), "statements": ts})
		return 0
	}
	fmt.Fprint(out, touch.Summary(ts, *k, plainProse))
	return 0
}

// forkPoint is where the current branch left the base branch, or HEAD on
// the base branch itself, so the change is the branch's commits and the
// working tree.
func forkPoint(dir string) string {
	run := func(args ...string) string {
		b, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	main := baseBranch()
	for _, ref := range []string{"origin/" + main, main} {
		if mb := run("merge-base", "HEAD", ref); mb != "" {
			return mb
		}
	}
	if run("rev-parse", "--verify", "-q", "HEAD") != "" {
		return "HEAD"
	}
	// No commit yet: everything is new.
	return emptyTree
}

// emptyTree is git's empty tree, the base of a repository with no commits.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
