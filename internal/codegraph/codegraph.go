// Package codegraph reads a project's code through Grove's embedded engine
// (D-053, D-062): where a name is defined, as whole symbol bodies. Text that
// is not code stays with git grep.
package codegraph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	grove "github.com/provasign/grove/pkg/grove"

	"github.com/provasign/stele/internal/adjudicate"
)

// Graph is an open Grove index of one repository. It shares the
// repository's .grove/ index with Prism and the grove CLI.
type Graph struct {
	ctx context.Context
	eng *grove.Engine
}

// Open opens the repository's index, bringing it up to date first.
func Open(ctx context.Context, repo string) (*Graph, error) {
	root, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(root, ".grove")); os.IsNotExist(err) {
		ignore(root)
	}
	eng, err := grove.Open(ctx, grove.Config{RepoRoot: root, OneShot: true})
	if err != nil {
		return nil, err
	}
	if stale, err := eng.IndexNeedsRefresh(ctx); err != nil || stale {
		if _, err := eng.Index(ctx, root); err != nil {
			eng.Close()
			return nil, err
		}
	}
	return &Graph{ctx: ctx, eng: eng}, nil
}

// ignore keeps an index Stele creates out of git status, in the clone's
// own exclude file: the project's files are not touched.
func ignore(root string) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	if src, _ := os.ReadFile(p); slices.Contains(strings.Split(string(src), "\n"), ".grove/") {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n.grove/\n")
}

// Close releases the index.
func (g *Graph) Close() error { return g.eng.Close() }

var defining = []string{"function", "method", "constructor", "class", "interface", "type", "enum", "struct", "trait", "macro"}

// Define returns where name is defined: functions, methods and types whose
// own name is name, at most four.
func (g *Graph) Define(name string) []adjudicate.LineRange {
	syms, err := g.eng.Symbols(g.ctx, name, 50)
	if err != nil {
		return nil
	}
	var out []adjudicate.LineRange
	for _, s := range syms {
		if s.Name != name || !slices.Contains(defining, string(s.Kind)) || len(out) == 4 {
			continue
		}
		out = append(out, adjudicate.LineRange{File: filepath.ToSlash(s.FilePath), From: s.Span.Start, To: s.Span.End})
	}
	return out
}

// Symbol is a function, method, type or field as Grove parses it, with the
// names it calls and the attributes it reads or writes.
type Symbol struct {
	Name, Qualified, Parent, Kind, File string
	Line                                int
	Calls, Attrs                        []string
}

// OpenParser opens Grove's parser for repo without indexing it, for reading
// the symbols of file contents that need not be on disk.
func OpenParser(ctx context.Context, repo string) (*Graph, error) {
	root, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(root, ".grove")); os.IsNotExist(err) {
		ignore(root)
	}
	eng, err := grove.Open(ctx, grove.Config{RepoRoot: root, OneShot: true})
	if err != nil {
		return nil, err
	}
	return &Graph{ctx: ctx, eng: eng}, nil
}

// Changed returns the symbols that differ between two contents of one
// repository-relative file (either may be empty): added, removed, or with a
// changed body or signature.
func (g *Graph) Changed(path string, before, after []byte) ([]Symbol, error) {
	parse := func(src []byte) ([]grove.Symbol, error) {
		if len(src) == 0 {
			return nil, nil
		}
		return g.eng.PreviewFileSymbols(path, src)
	}
	b, err := parse(before)
	if err != nil {
		return nil, err
	}
	a, err := parse(after)
	if err != nil {
		return nil, err
	}
	d := grove.Diff(b, a)
	var out []Symbol
	add := func(s *grove.Symbol) {
		if s == nil {
			return
		}
		x := Symbol{Name: s.Name, Qualified: s.QualifiedName, Parent: s.ParentSymbol, Kind: string(s.Kind), File: filepath.ToSlash(s.FilePath), Line: s.Span.Start}
		for _, c := range s.CallSites {
			x.Calls = append(x.Calls, c.Callee)
		}
		for _, c := range s.AttrSites {
			x.Attrs = append(x.Attrs, c.Callee)
		}
		out = append(out, x)
	}
	for i := range d.Added {
		add(&d.Added[i])
	}
	for i := range d.Removed {
		add(&d.Removed[i])
	}
	for _, c := range d.Changed {
		if c.After != nil {
			add(c.After)
		} else {
			add(c.Before)
		}
	}
	return out, nil
}
