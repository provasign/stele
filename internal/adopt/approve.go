package adopt

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/ir"
)

// ApproveRules records the owner's approval on each rule, pinned to the
// statement text the rule was translated from and to the rule's reading
// (D-056, D-057). Editing the statement, or reading it again into a
// different rule, needs approving again. Each approval is also appended to
// the ledger next to the rule's file.
func ApproveRules(reg *ir.Registry, ids []string, by string) error {
	if strings.TrimSpace(by) == "" {
		return fmt.Errorf("approval needs the approver's name")
	}
	if err := EditRules(reg, ids, func(x *ir.Rule, root *yaml.Node) (bool, error) {
		source := mappingValue(root, "source")
		if source == nil {
			source = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "source"}, source)
		}
		if source.Kind != yaml.MappingNode {
			return false, fmt.Errorf("rule %s: source is not a mapping", x.ID)
		}
		setString(source, "approvedBy", by)
		setString(source, "approvedHash", x.TextHash)
		setString(source, "approvedReading", x.ReadingHash())
		return false, nil
	}); err != nil {
		return err
	}
	return recordApprovals(reg, ids, by, time.Now().UTC())
}

// LedgerDir holds the record of approvals, next to the file that declares
// the rule: one file per approval run, one JSON line per rule approved. It
// lets a wrong rule be traced to the approval that let it in, and what that
// approval saw. One file per run, not one shared file, so approvals on
// parallel branches merge without conflict. (Registries from before keep a
// single approvals.jsonl, which stays as it is.)
const LedgerDir = "approvals"

// LedgerEntry is one approval of one rule.
type LedgerEntry struct {
	At          string `json:"at"` // RFC 3339, UTC
	By          string `json:"by"`
	Rule        string `json:"rule"`
	Version     int    `json:"version"`
	Statement   string `json:"statement,omitempty"`
	TextHash    string `json:"text_hash,omitempty"`
	ReadingHash string `json:"reading_hash"`
	Reading     string `json:"reading"` // ir.Rule.Reading: kind|effect|operation|on|subject|assume|when|assert|overrides
}

func recordApprovals(reg *ir.Registry, ids []string, by string, at time.Time) error {
	byDir := map[string][]LedgerEntry{}
	var dirs []string
	for _, id := range ids {
		x := reg.Rule(id)
		file := x.File
		if i := strings.LastIndexByte(file, ':'); i > 0 {
			file = file[:i]
		}
		dir := filepath.Dir(file)
		if byDir[dir] == nil {
			dirs = append(dirs, dir)
		}
		byDir[dir] = append(byDir[dir], LedgerEntry{At: at.Format(time.RFC3339), By: by, Rule: x.ID, Version: x.Version,
			Statement: x.StatementID, TextHash: x.TextHash, ReadingHash: x.ReadingHash(), Reading: x.Reading()})
	}
	sum := sha256.Sum256([]byte(strings.Join(ids, "\x00") + "\x00" + by))
	name := fmt.Sprintf("%s-%x.jsonl", at.Format("2006-01-02T150405Z"), sum[:3])
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, LedgerDir), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(dir, LedgerDir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(f)
		enc.SetEscapeHTML(false) // readable conditions
		for _, e := range byDir[dir] {
			if err := enc.Encode(e); err != nil {
				f.Close()
				return err
			}
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

// RemoveRules deletes the documents of the given rules from their files.
// Only a change preview uses it, to put a new version of a rule in place
// of the old one; a registry change retires rules instead.
func RemoveRules(reg *ir.Registry, ids []string) error {
	return EditRules(reg, ids, func(*ir.Rule, *yaml.Node) (bool, error) { return true, nil })
}

// EditRules rewrites the files declaring the given rules, calling edit on
// each rule's document; edit returns true to drop the document. YAML nodes
// keep the other documents, keys and comments.
func EditRules(reg *ir.Registry, ids []string, edit func(*ir.Rule, *yaml.Node) (bool, error)) error {
	byFile := map[string]map[string]bool{}
	for _, id := range ids {
		x := reg.Rule(id)
		if x == nil {
			return fmt.Errorf("no rule %s", id)
		}
		file := x.File
		if i := strings.LastIndexByte(file, ':'); i > 0 {
			file = file[:i]
		}
		if byFile[file] == nil {
			byFile[file] = map[string]bool{}
		}
		byFile[file][id] = true
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var out bytes.Buffer
		dec, enc := yaml.NewDecoder(bytes.NewReader(src)), yaml.NewEncoder(&out)
		enc.SetIndent(2)
		found := map[string]bool{}
		kept := 0
		for {
			var doc yaml.Node
			if err := dec.Decode(&doc); err == io.EOF {
				break
			} else if err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			if len(doc.Content) == 1 {
				if id := mappingValue(doc.Content[0], "id"); id != nil && byFile[file][id.Value] {
					found[id.Value] = true
					drop, err := edit(reg.Rule(id.Value), doc.Content[0])
					if err != nil {
						return fmt.Errorf("%s: %w", file, err)
					}
					if drop {
						continue
					}
				}
			}
			if err := enc.Encode(&doc); err != nil {
				return err
			}
			kept++
		}
		if kept == 0 {
			out.Reset() // every document dropped: an encoder with none fails to close
		} else if err := enc.Close(); err != nil {
			return err
		}
		for id := range byFile[file] {
			if !found[id] {
				return fmt.Errorf("%s: could not find rule %s", file, id)
			}
		}
		if err := os.WriteFile(file, out.Bytes(), 0o644); err != nil {
			return err
		}
	}
	return nil
}
