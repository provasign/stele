package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/translate"
)

// cmdKeep keeps a request declared with `shale intent` as a requirement:
// Stele reads it as part of the project's specs, checks it against the
// approved ones and puts it to the owner for approval. A request not kept
// is a task, what the agent is about to do, and Stele says nothing about
// it. With no request named, it keeps the newest one declared.
func cmdKeep(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("keep", flag.ExitOnError)
	project := fs.String("project", ".", "the repository")
	drop := fs.Bool("drop", false, "make a kept request a task again")
	by := fs.String("by", "", "who keeps it: agent, or the owner (default: git user.name)")
	// Flags may follow the request named: parse them wherever they are.
	var named []string
	for rest := args; ; {
		_ = fs.Parse(rest)
		if fs.NArg() == 0 {
			break
		}
		named, rest = append(named, fs.Arg(0)), fs.Args()[1:]
	}
	if *by == "" {
		*by = owner()
	}
	reqs, err := importer.ShaleRequests(*project)
	if err != nil {
		return failf("keep", err)
	}
	if len(reqs) == 0 {
		return failf("keep", fmt.Errorf("no request declared with `shale intent` here"))
	}
	var pick []translate.Statement
	if len(named) == 0 {
		pick = reqs[len(reqs)-1:] // in the order they were declared
	}
	for _, which := range named {
		var found []translate.Statement
		for _, r := range reqs {
			id := strings.TrimPrefix(r.ID, "intent/")
			title, _, _ := strings.Cut(r.Prose, "\n")
			if r.ID == which || id == which || strings.HasPrefix(strings.ToLower(title), strings.ToLower(which)) {
				found = append(found, r)
			}
		}
		if len(found) != 1 {
			var ids []string
			for _, f := range found {
				ids = append(ids, f.ID)
			}
			return failf("keep", fmt.Errorf("%q matches %d requests (%s); name one by its ID or the start of its title", which, len(found), strings.Join(ids, ", ")))
		}
		pick = append(pick, found[0])
	}
	var add []importer.KeptIntent
	var ids []string
	at := time.Now().UTC().Format("2006-01-02")
	for _, r := range pick {
		title, _, _ := strings.Cut(r.Prose, "\n")
		add = append(add, importer.KeptIntent{ID: r.ID, Title: title, By: *by, At: at})
		ids = append(ids, r.ID)
	}
	if *drop {
		err = importer.KeepIntents(*project, nil, ids)
	} else {
		err = importer.KeepIntents(*project, add, nil)
	}
	if err != nil {
		return failf("keep", err)
	}
	for _, k := range add {
		if *drop {
			fmt.Fprintf(out, "%q (%s) is a task again: Stele does not read it.\n", k.Title, k.ID)
		} else {
			fmt.Fprintf(out, "Kept %q (%s) as a requirement.\n", k.Title, k.ID)
		}
	}
	if !*drop {
		fmt.Fprintln(out, "Next: run `stele change shale . --base <branch> --json` and put its questions to the owner; commit "+importer.KeptIntentsFile+" with the change.")
	} else {
		fmt.Fprintln(out, "Commit "+importer.KeptIntentsFile+" with the change.")
	}
	return 0
}
