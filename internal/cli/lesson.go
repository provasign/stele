package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lesson"
)

const lessonUsage = `usage: stele lesson add --kind term|fact|reading|scope --says TEXT [--statement ID] [--terms a,b] [--registry DIR]
       stele lesson list [--registry DIR]
       stele lesson remove <id> [--registry DIR]

Lessons are what the owner taught Stele about this project (D-063). Most come
from answers to stele change questions on their own; add one here for
something no question has asked. Each is a file under the registry's
lessons/, committed with the specs.
`

// cmdLesson adds, lists and removes the registry's lessons.
func cmdLesson(args []string, out io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, lessonUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("lesson "+sub, flag.ExitOnError)
	registry := fs.String("registry", "", "registry directory (default: stele.yaml's)")
	kind := fs.String("kind", "", "term (two names for one thing), fact (true of the world here), reading (how one requirement reads; needs --statement) or scope (how requirements here are read)")
	says := fs.String("says", "", "the lesson, in the owner's words, as the translator should follow it")
	statement := fs.String("statement", "", "pin the lesson to this statement as now written")
	terms := fs.String("terms", "", "comma-separated words for finding the requirements it concerns")
	by := fs.String("by", "", "who teaches it (default: git user.name)")
	var id string
	if sub == "remove" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		id, rest = rest[0], rest[1:]
	}
	_ = fs.Parse(rest)
	if *registry == "" {
		cfg, err := config.Find(".")
		if err != nil {
			return failf("lesson", err)
		}
		if cfg == nil || cfg.Registry == "" {
			return failf("lesson", fmt.Errorf("no stele.yaml with a registry here: pass --registry"))
		}
		*registry = cfg.RegistryDir()
	}
	switch sub {
	case "add":
		l := ir.Lesson{Kind: *kind, Says: *says, From: "stele lesson add", By: *by, At: time.Now().UTC().Format("2006-01-02")}
		if l.By == "" {
			l.By = owner()
		}
		for _, w := range strings.Split(*terms, ",") {
			if w = strings.TrimSpace(w); w != "" {
				l.Terms = append(l.Terms, w)
			}
		}
		if *statement != "" {
			reg, err := ir.LoadDir(*registry)
			if err != nil {
				return failf("lesson", err)
			}
			h := ""
			for _, x := range reg.Rules {
				if x.StatementID == *statement && x.Live() {
					if h = x.StatementHash; h == "" {
						h = x.TextHash
					}
					break
				}
			}
			if h == "" {
				return failf("lesson", fmt.Errorf("no live rule of statement %s in %s to pin the lesson to", *statement, *registry))
			}
			l.Statements = map[string]string{*statement: h}
		}
		id, added, err := lesson.Add(*registry, l)
		if err != nil {
			return failf("lesson", err)
		}
		if !added {
			fmt.Fprintf(out, "%s is already a lesson here.\n", id)
			return 0
		}
		fmt.Fprintf(out, "Saved %s in %s/%s.yaml. Commit it with the specs.\n", id, lessonDirOf(*registry), id)
		return 0
	case "list":
		ls, err := lesson.Load(*registry)
		if err != nil {
			return failf("lesson", err)
		}
		current := map[string]string{}
		if reg, err := ir.LoadDir(*registry); err == nil {
			for _, x := range reg.Rules {
				if x.StatementID != "" && x.Live() {
					h := x.StatementHash
					if h == "" {
						h = x.TextHash
					}
					current[x.StatementID] = h
				}
			}
		}
		stale := map[string]bool{}
		for _, l := range lesson.Stale(ls, current) {
			stale[l.ID] = true
		}
		sort.SliceStable(ls, func(i, j int) bool { return ls[i].ID < ls[j].ID })
		for _, l := range ls {
			scope := "project-wide"
			if l.Pinned() {
				var ss []string
				for s := range l.Statements {
					ss = append(ss, s)
				}
				sort.Strings(ss)
				scope = strings.Join(ss, ", ")
			}
			mark := ""
			if stale[l.ID] {
				mark = " [stale: the requirement changed since; not applied]"
			}
			fmt.Fprintf(out, "%s %s (%s)%s\n    %s\n    from %s, by %s, %s\n", l.ID, l.Kind, scope, mark, l.Says, l.From, l.By, l.At)
		}
		for _, p := range lesson.Contradictions(ls) {
			fmt.Fprintf(out, "warning: %s and %s concern the same terms and say different things\n", p[0].ID, p[1].ID)
		}
		fmt.Fprintf(out, "%d lesson(s)\n", len(ls))
		return 0
	case "remove":
		if id == "" {
			fmt.Fprint(os.Stderr, lessonUsage)
			return 2
		}
		if err := lesson.Remove(*registry, id); err != nil {
			return failf("lesson", err)
		}
		fmt.Fprintf(out, "Removed %s. Commit the deletion with the specs.\n", id)
		return 0
	}
	fmt.Fprint(os.Stderr, lessonUsage)
	return 2
}

func lessonDirOf(registry string) string { return strings.TrimSuffix(registry, "/") + "/" + lesson.Dir }
