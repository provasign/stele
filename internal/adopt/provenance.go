package adopt

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/ir"
)

// UpdateSources persists sync's current statement identities without renaming
// rules or changing their versions. YAML nodes preserve other metadata and
// comments, including owner approvals and the original translated prose/hash.
func UpdateSources(reg *ir.Registry, updates map[string]RuleSource) error {
	byFile := map[string]map[string]RuleSource{}
	for id, update := range updates {
		x := reg.Rule(id)
		if x == nil {
			return fmt.Errorf("no rule %s", id)
		}
		file := x.File
		if i := strings.LastIndexByte(file, ':'); i > 0 {
			file = file[:i]
		}
		if byFile[file] == nil {
			byFile[file] = map[string]RuleSource{}
		}
		byFile[file][id] = update
	}
	files := make([]string, 0, len(byFile))
	for file := range byFile {
		files = append(files, file)
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
		for {
			var doc yaml.Node
			if err := dec.Decode(&doc); err == io.EOF {
				break
			} else if err != nil {
				return err
			}
			if len(doc.Content) == 1 {
				root := doc.Content[0]
				if id := mappingValue(root, "id"); id != nil {
					if update, ok := byFile[file][id.Value]; ok {
						source := mappingValue(root, "source")
						if source == nil {
							source = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
							root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "source"}, source)
						}
						if source.Kind != yaml.MappingNode {
							return fmt.Errorf("%s: rule %s source is not a mapping", file, id.Value)
						}
						setString(source, "statementId", update.StatementID)
						setString(source, "statementHash", update.Hash)
						setString(source, "doc", update.Doc)
						found[id.Value] = true
					}
				}
			}
			if err := enc.Encode(&doc); err != nil {
				return err
			}
		}
		if err := enc.Close(); err != nil {
			return err
		}
		for id := range byFile[file] {
			if !found[id] {
				return fmt.Errorf("%s: could not find rule %s to update its source", file, id)
			}
		}
		if err := os.WriteFile(file, out.Bytes(), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				return n.Content[i+1]
			}
		}
	}
	return nil
}

func setString(n *yaml.Node, key, value string) {
	if old := mappingValue(n, key); old != nil {
		old.Kind, old.Tag, old.Value = yaml.ScalarNode, "!!str", value
		old.Content = nil
	} else {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}
}
