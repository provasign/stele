package change

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// cacheFile keeps a change's translation between runs, so a statement whose
// text and notes have not changed keeps its rules (ISSUES 51) and the terms
// the change proposed stay what they were.
const cacheFile = "translation.json"

type cache struct {
	// Digest names the registry vocabulary the entries were translated over;
	// when it changes, everything is translated again.
	Digest   string                `json:"digest"`
	Entries  map[string]entry      `json:"entries"`
	Proposal vocab.Proposal        `json:"proposal"`
	Links    []translate.Candidate `json:"links,omitempty"`
	// Flags are new terms consolidation could not fold into an existing
	// one by itself; Merges are those it did.
	Flags  []vocab.Flag  `json:"flags,omitempty"`
	Merges []vocab.Merge `json:"merges,omitempty"`
}

type entry struct {
	Key   string                `json:"key"`
	Cands []translate.Candidate `json:"candidates"`
	// Findings are what the coverage and self checks found in the reading:
	// clauses no rule checks, rules broader than the text, rules that block
	// a step. The owner sees them beside the reading before approving.
	Findings []string `json:"findings,omitempty"`
}

// key identifies what a statement was translated from: its text and the
// notes it was given.
func key(s translate.Statement) string {
	h := sha256.Sum256([]byte(s.Prose + "\x00" + strings.Join(s.Notes, "\x00")))
	return fmt.Sprintf("%x", h[:12])
}

func loadCache(dir, digest string) (*cache, error) {
	c := &cache{Digest: digest, Entries: map[string]entry{}}
	src, err := os.ReadFile(filepath.Join(dir, cacheFile))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var old cache
	if err := json.Unmarshal(src, &old); err != nil {
		return nil, fmt.Errorf("%s: %w", cacheFile, err)
	}
	if old.Digest != digest {
		return c, nil
	}
	if old.Entries == nil {
		old.Entries = map[string]entry{}
	}
	return &old, nil
}

func (c *cache) save(dir string) error {
	js, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, cacheFile), append(js, '\n'), 0o644)
}

// addLinks adds world facts not already known, by their text.
func (c *cache) addLinks(links []translate.Candidate) {
	for _, l := range links {
		if !slices.ContainsFunc(c.Links, func(k translate.Candidate) bool { return k.When == l.When && k.Assert == l.Assert }) {
			c.Links = append(c.Links, l)
		}
	}
}

// digestFiles fingerprints the vocabulary files and the context new terms go to.
func digestFiles(files []string, context string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00", context)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.Base(f), len(src))
		h.Write(src)
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:12]), nil
}

// dropAttr removes the attribute Entity.name from the proposal, and the
// entity too when it is proposed and nothing else is left on it.
func dropAttr(p vocab.Proposal, qname, into string) vocab.Proposal {
	ent, name, _ := strings.Cut(qname, ".")
	strip := func(es []vocab.Entity, keepEmpty bool) []vocab.Entity {
		var out []vocab.Entity
		for _, e := range es {
			if e.Name == ent {
				e.Attributes = slices.DeleteFunc(slices.Clone(e.Attributes), func(a vocab.Attribute) bool { return a.Name == name })
				if !keepEmpty && len(e.Attributes) == 0 && len(e.Relations) == 0 {
					continue
				}
			}
			out = append(out, e)
		}
		return out
	}
	p.Entities = strip(p.Entities, true)
	p.Extensions = strip(p.Extensions, false)
	return renameWrites(p, map[string]string{qname: into})
}

// renameWrites points the operations' writes at the new name of each
// attribute in renamed, or drops the write when the new name is empty.
func renameWrites(p vocab.Proposal, renamed map[string]string) vocab.Proposal {
	ops := slices.Clone(p.Operations)
	for i, o := range ops {
		var ws []string
		for _, w := range o.Writes {
			if to, ok := renamed[w]; ok {
				w = to
			}
			if w != "" && !slices.Contains(ws, w) {
				ws = append(ws, w)
			}
		}
		ops[i].Writes = ws
	}
	p.Operations = ops
	return p
}

// prune keeps the proposed attributes and operations some rule names, and
// the entities holding them or named by a rule or a kept relation. A rule is
// matched by the words of its text, so a symbol it may name is kept.
func prune(p vocab.Proposal, cands []translate.Candidate) vocab.Proposal {
	words := map[string]bool{}
	ops := map[string]bool{}
	for _, c := range cands {
		if c.Outcome != "rule" {
			continue
		}
		for _, t := range []string{c.When, c.Assert, c.Subject, c.On, c.Operation} {
			for _, w := range strings.FieldsFunc(t, func(r rune) bool {
				return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
			}) {
				words[w] = true
			}
		}
		ops[c.On], ops[c.Operation] = true, true
	}
	dropped := map[string]string{}
	keepAttrs := func(es []vocab.Entity) []vocab.Entity {
		var out []vocab.Entity
		for _, e := range es {
			e.Attributes = slices.DeleteFunc(slices.Clone(e.Attributes), func(a vocab.Attribute) bool {
				if !words[a.Name] {
					dropped[e.Name+"."+a.Name] = ""
					return true
				}
				return false
			})
			out = append(out, e)
		}
		return out
	}
	p.Entities, p.Extensions = keepAttrs(p.Entities), keepAttrs(p.Extensions)
	p = renameWrites(p, dropped)
	p.Operations = slices.DeleteFunc(slices.Clone(p.Operations), func(o vocab.Operation) bool { return !ops[o.Name] })
	used := map[string]bool{}
	for _, o := range p.Operations {
		ent, _, _ := strings.Cut(o.Name, ".")
		used[ent] = true
	}
	for _, e := range append(append([]vocab.Entity{}, p.Entities...), p.Extensions...) {
		if len(e.Attributes) > 0 || words[e.Name] || words[lowerFirst(e.Name)] {
			used[e.Name] = true
		}
	}
	for changed := true; changed; { // a kept entity keeps its relations' targets
		changed = false
		for _, e := range append(append([]vocab.Entity{}, p.Entities...), p.Extensions...) {
			if !used[e.Name] {
				continue
			}
			for _, r := range e.Relations {
				if !used[r.To] {
					used[r.To], changed = true, true
				}
			}
		}
	}
	p.Entities = slices.DeleteFunc(p.Entities, func(e vocab.Entity) bool { return !used[e.Name] })
	p.Extensions = slices.DeleteFunc(p.Extensions, func(e vocab.Entity) bool {
		return len(e.Attributes) == 0 && (len(e.Relations) == 0 || !used[e.Name])
	})
	return p
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// terms lists the proposal's symbols: entities, attributes and operations.
func terms(p vocab.Proposal) []string {
	var out []string
	for _, e := range p.Entities {
		out = append(out, e.Name)
	}
	for _, e := range append(append([]vocab.Entity{}, p.Entities...), p.Extensions...) {
		for _, a := range e.Attributes {
			out = append(out, e.Name+"."+a.Name)
		}
	}
	for _, o := range p.Operations {
		out = append(out, o.Name)
	}
	slices.Sort(out)
	return slices.Compact(out)
}
