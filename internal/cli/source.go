package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/lsp"
	"github.com/provasign/stele/internal/specsync"
)

type sourceFlags struct {
	project, root, format, snapshot, registry *string
	anyCase                                   *bool
	fs                                        *flag.FlagSet
}

func sourceOptions(fs *flag.FlagSet) sourceFlags {
	return sourceFlags{fs.String("project", "", "project directory containing stele.yaml"), fs.String("specs", "", "spec directory (repository root for speckit, openspec and kiro)"), fs.String("format", "", "markdown, speckit, openspec or kiro"), fs.String("old", "", "accepted statements snapshot"), fs.String("registry", "", "live rule registry"), fs.Bool("any-case", false, "markdown: include lowercase normative keywords"), fs}
}
func (f sourceFlags) resolve(workspace string) (specsync.Options, error) {
	project := *f.project
	if project == "" {
		project = workspace
	}
	if project == "" {
		project = "."
	}
	project, err := filepath.Abs(project)
	if err != nil {
		return specsync.Options{}, err
	}
	cfg, err := config.Find(project)
	if err != nil {
		return specsync.Options{}, err
	}
	o := specsync.Options{Format: "markdown"}
	if cfg != nil {
		o.Registry = cfg.RegistryDir()
		if cfg.Specs != nil {
			o.Root = cfg.SpecsDir()
			o.Snapshot = cfg.SnapshotFile()
			o.Format = cfg.Specs.Format
			o.AnyCase = cfg.Specs.AnyCase
		}
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(project, p)
	}
	if *f.root != "" {
		o.Root = resolve(*f.root)
	}
	if *f.snapshot != "" {
		o.Snapshot = resolve(*f.snapshot)
	}
	if *f.registry != "" {
		o.Registry = resolve(*f.registry)
	}
	if *f.format != "" {
		o.Format = *f.format
	}
	if flagSet(f.fs, "any-case") {
		o.AnyCase = *f.anyCase
	}
	if err := o.Validate(); err != nil {
		return o, fmt.Errorf("%w; configure specs in stele.yaml or pass --specs, --old and --registry", err)
	}
	return o, nil
}

func cmdWatch(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	source := sourceOptions(fs)
	once := fs.Bool("once", false, "inspect saved specs once (for coding-tool hooks)")
	check := fs.Bool("check", false, "inspect once and exit 1 while sources and rules need synchronization")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON (one object per update)")
	queue := fs.String("out", "", "atomically write added/changed statements to a translation queue")
	interval := fs.Duration("interval", 500*time.Millisecond, "poll interval; content must stabilize before emitting updates")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	opt, err := source.resolve("")
	if err != nil {
		return failf("watch", err)
	}
	if *interval <= 0 {
		return failf("watch", fmt.Errorf("interval must be positive"))
	}
	onceMode := *once || *check
	emit := func(r *specsync.Result, err error) {
		if err == nil && *queue != "" && !onceMode {
			err = specsync.WritePending(*queue, r, opt)
		}
		if err != nil {
			if *jsonOut {
				_ = json.NewEncoder(out).Encode(map[string]string{"error": err.Error()})
			} else {
				fmt.Fprintf(out, "Stele sync error: %s\n", err)
			}
			return
		}
		if *jsonOut {
			_ = json.NewEncoder(out).Encode(r)
			return
		}
		c := r.Changes
		fmt.Fprintf(out, "spec sync: %d added, %d changed, %d moved, %d removed, %d ambiguous; %d stale, %d orphaned rules; in sync: %t\n", len(c.Added), len(c.Changed), len(c.Moved), len(c.Removed), len(c.Ambiguous), len(r.StaleRules), len(r.OrphanRules), r.InSync)
		for _, d := range r.Diagnostics {
			fmt.Fprintf(out, "%s:%d: %s: %s\n", d.File, d.Line, d.Code, d.Message)
		}
	}
	if *once || *check {
		r, err := specsync.Evaluate(opt)
		if err != nil {
			emit(nil, err)
			return 2
		}
		// A requirement that only moved in its file needs no check.
		if len(r.Pending)+len(r.StaleRules)+len(r.OrphanRules)+len(r.Changes.Removed) > 0 {
			// The check that takes the new statements, so a hook can say it.
			dir := *source.project
			if dir == "" {
				dir = "."
			}
			if cfg, err := config.Find(dir); err == nil && cfg != nil {
				if c := nextCommand(cfg, dir); !strings.HasPrefix(c, "stele watch") {
					r.Next = "run `" + c + "` before editing code, and follow it"
				}
			}
		}
		if *queue != "" {
			if err := specsync.WritePending(*queue, r, opt); err != nil {
				return failf("watch", err)
			}
		}
		emit(r, nil)
		if *check && !r.InSync {
			return 1
		}
		return 0
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := specsync.Watch(ctx, opt, *interval, emit); err != nil {
		return failf("watch", err)
	}
	return 0
}

func cmdLSP(args []string, in io.Reader, out io.Writer) int {
	fs := flag.NewFlagSet("lsp", flag.ContinueOnError)
	source := sourceOptions(fs)
	interval := fs.Duration("interval", 500*time.Millisecond, "saved-file polling interval")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *interval <= 0 {
		return failf("lsp", fmt.Errorf("interval must be positive"))
	}
	server := lsp.Server{Resolve: source.resolve, Interval: *interval}
	if err := server.Serve(context.Background(), in, out); err != nil {
		return failf("lsp", err)
	}
	return 0
}
