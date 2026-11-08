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
	decline := fs.Bool("decline", false, "the owner said the request is not a requirement: set it aside for good, never to be offered again")
	list := fs.Bool("list", false, "list the requests with their state: kept, set aside, or a task not yet offered")
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
	if *list {
		return listRequests(*project, reqs, out)
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
	switch {
	case *decline:
		// The owner's no is final: the request leaves the kept list and is
		// set aside as not a requirement, so no agent offers it again.
		var set []importer.RetiredIntent
		for _, k := range add {
			set = append(set, importer.RetiredIntent{ID: k.ID, By: *by, At: at, Why: "not a requirement"})
		}
		if err = importer.RetireIntents(*project, set); err == nil {
			err = importer.KeepIntents(*project, nil, ids)
		}
	case *drop:
		err = importer.KeepIntents(*project, nil, ids)
	default:
		err = importer.KeepIntents(*project, add, nil)
	}
	if err != nil {
		return failf("keep", err)
	}
	for _, k := range add {
		switch {
		case *decline:
			fmt.Fprintf(out, "%q (%s) is set aside as not a requirement; it is not offered again.\n", k.Title, k.ID)
		case *drop:
			fmt.Fprintf(out, "%q (%s) is a task again: Stele does not read it.\n", k.Title, k.ID)
		default:
			fmt.Fprintf(out, "Kept %q (%s) as a requirement.\n", k.Title, k.ID)
		}
	}
	switch {
	case *decline:
		fmt.Fprintln(out, "Commit "+importer.RetiredIntentsFile+" with the change.")
	case *drop:
		fmt.Fprintln(out, "Commit "+importer.KeptIntentsFile+" with the change.")
	default:
		fmt.Fprintln(out, "Next: run `stele change shale . --base <branch> --json` and put its questions to the owner; commit "+importer.KeptIntentsFile+" with the change.")
	}
	return 0
}

// listRequests prints every request with its state, newest last: kept (a
// requirement), set aside (the owner said it is not one, or a later request
// replaced it), or a task (never offered, or not yet). The agent reads it
// to offer a request once and never again.
func listRequests(project string, reqs []translate.Statement, out io.Writer) int {
	ks, err := importer.KeptIntents(project)
	if err != nil {
		return failf("keep", err)
	}
	rs, err := importer.RetiredIntents(project)
	if err != nil {
		return failf("keep", err)
	}
	state := map[string]string{}
	for _, r := range rs {
		state[r.ID] = "set aside"
	}
	for _, k := range ks {
		state[k.ID] = "kept"
	}
	// ShaleRequests leaves out retired requests; list them too.
	all, err := importer.ShaleRequestsAll(project)
	if err == nil {
		reqs = all
	}
	if len(reqs) == 0 && len(rs) == 0 {
		fmt.Fprintln(out, "No request declared with `shale intent` here.")
		return 0
	}
	for _, r := range reqs {
		st := state[r.ID]
		if st == "" {
			st = "task"
		}
		title, _, _ := strings.Cut(r.Prose, "\n")
		fmt.Fprintf(out, "%-10s %s  %s\n", st, r.ID, title)
	}
	return 0
}
