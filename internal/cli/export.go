package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/provasign/stele/internal/adopt"
	"github.com/provasign/stele/internal/card"
	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
)

// exportCapability is the OpenSpec capability the kept requests become.
const exportCapability = "requests"

// cmdExport writes a Shale project's kept requests as its first OpenSpec
// spec, so a project that grew its requirements one request at a time can
// adopt a spec tool without writing a document by hand: each kept request
// becomes a requirement under openspec/specs/requests/spec.md, the
// approved rules follow their statements under the new IDs, stele.yaml
// names the OpenSpec format, and the accepted statements are rewritten.
// Shale stays as the record of tasks.
func cmdExport(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	project := fs.String("project", ".", "the repository")
	if len(args) == 0 || args[0] != "openspec" {
		fmt.Fprint(os.Stderr, "usage: stele export openspec [--project DIR]\n")
		return 2
	}
	_ = fs.Parse(args[1:])
	cfg, err := config.Find(*project)
	if err != nil {
		return failf("export", err)
	}
	if cfg == nil || cfg.Specs == nil || cfg.Specs.Format != "shale" {
		return failf("export", fmt.Errorf("stele.yaml does not name Shale requests as the specs (specs.format: shale); nothing to export"))
	}
	root := cfg.Dir()
	kept, err := importer.Shale(root)
	if err != nil {
		return failf("export", err)
	}
	if len(kept) == 0 {
		return failf("export", fmt.Errorf("no request is kept as a requirement (stele keep); nothing to export"))
	}
	specPath := filepath.Join(root, "openspec", "specs", exportCapability, "spec.md")
	if _, err := os.Stat(specPath); err == nil {
		return failf("export", fmt.Errorf("%s exists; export writes the first spec only", specPath))
	}
	// Titles must be distinct: OpenSpec names a requirement by its title.
	// matched carries each request with the text OpenSpec reads it as, so
	// the rules follow it as the same statement under a new ID.
	renamed := map[string]string{}
	var matched []translate.Statement
	seen := map[string]int{}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n## Purpose\n\nRequirements kept from the requests the coding agents declared with `shale intent` (`stele keep`), exported by Stele on %s. Edit them here from now on; Shale keeps recording the tasks.\n\n## Requirements\n", strings.ToUpper(exportCapability[:1])+exportCapability[1:], time.Now().UTC().Format("2006-01-02"))
	for _, s := range kept {
		title, body, _ := strings.Cut(strings.TrimSpace(s.Prose), "\n")
		title = strings.TrimSpace(strings.TrimPrefix(title, "Requirement:"))
		if seen[title]++; seen[title] > 1 {
			title = fmt.Sprintf("%s (%d)", title, seen[title])
		}
		renamed[s.ID] = exportCapability + "/" + title
		fmt.Fprintf(&b, "\n### Requirement: %s\n", title)
		prose := "Requirement: " + title
		if body = strings.TrimSpace(body); body != "" {
			b.WriteString(body + "\n")
			prose += "\n" + body
		}
		matched = append(matched, translate.Statement{ID: s.ID, Source: s.Source, Prose: prose})
	}
	if err := os.MkdirAll(filepath.Dir(specPath), 0o755); err != nil {
		return failf("export", err)
	}
	if err := os.WriteFile(specPath, []byte(b.String()), 0o644); err != nil {
		return failf("export", err)
	}
	cur, err := importer.OpenSpec(root, false)
	if err != nil {
		return failf("export", err)
	}
	// The approved rules follow their statements: same body, new ID.
	reg, err := ir.LoadDir(cfg.RegistryDir())
	if err != nil {
		return failf("export", err)
	}
	rep := adopt.SyncRenamed(matched, cur, reg, renamed)
	if len(rep.Sources) > 0 {
		if err := adopt.UpdateSources(reg, rep.Sources); err != nil {
			return failf("export", err)
		}
	}
	moved := make([]string, 0, len(rep.Moved))
	for from := range rep.Moved {
		moved = append(moved, from)
	}
	sort.Strings(moved)
	for _, from := range moved {
		fmt.Fprintf(out, "%s -> %s\n", card.Title(statementText(kept, from), from), rep.Moved[from])
	}
	cfg.Specs.Format, cfg.Specs.Root = "openspec", "."
	if err := config.Write(root, *cfg); err != nil {
		return failf("export", err)
	}
	if err := writeSnapshot(cfg.SnapshotFile(), cur, "openspec"); err != nil {
		return failf("export", err)
	}
	fmt.Fprintf(out, "Wrote %d requirement(s) to %s; %d rule(s) follow them; stele.yaml now names OpenSpec. Commit openspec/, stele.yaml, %s and the registry together. From now on, write spec changes under openspec/changes/ and check them with `stele change openspec . <change-id> --json`.\n",
		len(kept), rel(root, specPath), len(rep.Sources), rel(root, cfg.SnapshotFile()))
	return 0
}

func statementText(stmts []translate.Statement, id string) string {
	for _, s := range stmts {
		if s.ID == id {
			return s.Prose
		}
	}
	return ""
}

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(r)
	}
	return path
}
