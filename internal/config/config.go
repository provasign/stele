// Package config reads and writes stele.yaml, the project settings that
// `stele init` creates at the repository root (docs/04 repository layout).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the settings file at the repository root.
const FileName = "stele.yaml"

// Config is the content of stele.yaml.
type Config struct {
	FormatVersion int       `yaml:"formatVersion"`
	Registry      string    `yaml:"registry"` // registry directory, relative to stele.yaml
	Translate     Translate `yaml:"translate"`
	Vocab         Vocab     `yaml:"vocab,omitempty"`
	Specs         *Specs    `yaml:"specs,omitempty"`
	// Harnesses are the coding agents `stele init` set up to run Stele's
	// loop (claude, codex, cursor, windsurf, vscode, gemini, opencode).
	Harnesses []string `yaml:"harnesses,omitempty"`

	dir string // where stele.yaml was found
}

// Specs connects saved prose with the accepted statement snapshot. Paths are
// relative to stele.yaml; watching never advances the accepted snapshot.
type Specs struct {
	Root     string `yaml:"root"`
	Format   string `yaml:"format"` // markdown | speckit | openspec
	Snapshot string `yaml:"snapshot"`
	AnyCase  bool   `yaml:"anyCase,omitempty"`
}

// Dir is the directory stele.yaml is in: the project's root.
func (c *Config) Dir() string { return c.dir }

func (c *Config) SpecsDir() string     { return filepath.Join(c.dir, c.Specs.Root) }
func (c *Config) SnapshotFile() string { return filepath.Join(c.dir, c.Specs.Snapshot) }

// Translate holds the defaults for `stele translate`.
type Translate struct {
	LLM    string `yaml:"llm"`              // one backend: agent (the coding agent Stele runs from, and its model), anthropic, openai, claude, codex or cmd
	Model  string `yaml:"model,omitempty"`  // empty uses the agent's model (agent) or the backend's default
	Effort string `yaml:"effort,omitempty"` // reasoning effort; empty uses Stele's default for the backend
	Drafts int    `yaml:"drafts"`           // drafts per requirement; above 1, disagreements become questions
}

// Vocab holds the defaults for `stele vocab propose`. Finding the facts
// across specs is the hardest language step and only a few requests, so it
// can use a stronger model than translation.
type Vocab struct {
	LLM   string `yaml:"llm,omitempty"`   // empty uses translate.llm
	Model string `yaml:"model,omitempty"` // empty uses the backend's default
}

// Default is what `stele init --yes` writes.
func Default() Config {
	return Config{FormatVersion: 1, Registry: "specs/registry", Translate: Translate{LLM: "agent", Drafts: 3}}
}

// Find looks for stele.yaml in dir and its parents. It returns nil, nil when
// there is none.
func Find(dir string) (*Config, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		path := filepath.Join(dir, FileName)
		src, err := os.ReadFile(path)
		if err == nil {
			var c Config
			if err := yaml.Unmarshal(src, &c); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			if c.Translate.Drafts == 0 {
				c.Translate.Drafts = Default().Translate.Drafts // not written: the default, as stele init writes it
			}
			if err := c.Validate(); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			c.dir = dir
			return &c, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil
		}
		dir = parent
	}
}

// inProject reports whether a path stele.yaml names stays inside the
// project: relative, and not climbing out of it. A crafted stele.yaml
// could otherwise point an import, and the model calls that read it, at
// files elsewhere on the machine (a security review, 2026-11-11).
func inProject(p string) bool {
	if p == "" {
		return true
	}
	if filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return false
	}
	c := filepath.Clean(filepath.FromSlash(p))
	return c != ".." && !strings.HasPrefix(c, ".."+string(filepath.Separator))
}

// Validate checks the settings.
func (c *Config) Validate() error {
	if !inProject(c.Registry) {
		return fmt.Errorf("registry %q is outside the project: give a path inside the folder that holds stele.yaml", c.Registry)
	}
	if c.Specs != nil {
		if c.Specs.Root == "" || c.Specs.Snapshot == "" {
			return fmt.Errorf("specs.root and specs.snapshot are required")
		}
		for name, p := range map[string]string{"specs.root": c.Specs.Root, "specs.snapshot": c.Specs.Snapshot} {
			if !inProject(p) {
				return fmt.Errorf("%s %q is outside the project: give a path inside the folder that holds stele.yaml", name, p)
			}
		}
		switch c.Specs.Format {
		case "markdown", "speckit", "openspec", "kiro", "shale":
		default:
			return fmt.Errorf("specs.format must be markdown, speckit, openspec, kiro or shale")
		}
	}
	if c.FormatVersion != 1 {
		return fmt.Errorf("formatVersion %d, this stele reads 1", c.FormatVersion)
	}
	if c.Translate.Drafts < 1 {
		return fmt.Errorf("translate.drafts must be at least 1")
	}
	switch c.Translate.LLM {
	case "agent", "anthropic", "openai", "claude", "codex", "cmd":
	default:
		return fmt.Errorf("translate.llm %q: use agent, anthropic, openai, claude, codex or cmd", c.Translate.LLM)
	}
	return nil
}

// RegistryDir is the registry directory as a path usable from the working directory.
func (c *Config) RegistryDir() string { return filepath.Join(c.dir, c.Registry) }

// Write saves c as dir/stele.yaml.
func Write(dir string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	src, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# Stele settings. See docs/04-data-model.md for the registry layout.\n" +
		"# translate.drafts: drafts per requirement. 1 is cheapest. Above 1, readings are\n" +
		"# compared by the solver and disagreements become questions for the spec author.\n" +
		"# translate.llm: agent runs Stele's calls on Claude Code, the coding agent you run it\n" +
		"# from; claude (the CLI) and anthropic (an API key) name one.\n" +
		"# translate.model: the model that reads the specs. stele init sets claude-sonnet-5-5:\n" +
		"# Stele's times, costs and accuracy were measured on it, and a larger model reads\n" +
		"# several times slower for more money. Remove the line to use the model the agent\n" +
		"# is using, or name another.\n" +
		"# translate.effort: the model's reasoning effort (unset: the CLI's default).\n"
	return os.WriteFile(filepath.Join(dir, FileName), append([]byte(header), src...), 0o644)
}
