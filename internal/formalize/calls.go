package formalize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// CallStats counts backend invocations, including failures, across resumes.
// Interrupted means a durable start has no corresponding finish record. It
// does not assert whether the remote service completed or charged for a call.
type CallStats struct {
	Started     int `json:"started"`
	Completed   int `json:"completed"`
	Failed      int `json:"failed"`
	Interrupted int `json:"interrupted"`
}

type callEvent struct {
	ID           int       `json:"id"`
	Event        string    `json:"event"`
	At           time.Time `json:"at"`
	Backend      string    `json:"backend,omitempty"`
	PromptBytes  int       `json:"prompt_bytes,omitempty"`
	SchemaBytes  int       `json:"schema_bytes,omitempty"`
	ReplyBytes   int       `json:"reply_bytes,omitempty"`
	Milliseconds int64     `json:"elapsed_ms,omitempty"`
	Error        string    `json:"error,omitempty"`
	Usage        llm.Usage `json:"usage,omitempty"`
	Artifact     string    `json:"artifact,omitempty"`
}

type callJournal struct {
	llm.Client
	mu     sync.Mutex
	file   *os.File
	opt    Options
	counts CallStats
	dir    string
	stop   error   // budget, cancellation, deadline or journal failure stops later calls
	spent  float64 // reported cost of this run's calls, for MaxCost
	// For MaxCost: the prompt bytes of the calls that reported a cost, and
	// the expected cost of the calls in flight. A call starts only when the
	// spend, the calls in flight and its own expected cost fit the budget.
	pricedBytes float64
	pricedCost  float64
	inflight    int
	pending     float64
	// held are this run's calls, in memory, when only failed ones are
	// kept (Options.FailedCalls): written out by keepFailed.
	held     []heldCall
	unpriced bool // a call completed without a cost: the budget cannot apply
	done     *sync.Cond
}

type heldCall struct {
	id     int
	failed bool   // the call itself failed, not the run's own deadline
	text   string // what the request said, to match a failed statement's text
	data   []byte
}

// keepFailed writes the calls that failed, and those whose request read a
// statement the run could not translate, to Options.FailedCalls. A run
// with nothing failed keeps nothing.
func (j *callJournal) keepFailed(stmts []translate.Statement, res *Result) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.held) == 0 || j.opt.FailedCalls == "" {
		return nil
	}
	var failed []string
	if res != nil {
		prose := map[string]string{}
		for _, s := range stmts {
			prose[s.ID] = s.Prose
		}
		for _, e := range res.Ends {
			if e.State == Invalid && prose[e.ID] != "" {
				failed = append(failed, prose[e.ID])
			}
		}
	}
	var keep []heldCall
	for _, h := range j.held {
		if h.failed || slices.ContainsFunc(failed, func(p string) bool { return strings.Contains(h.text, p) }) {
			keep = append(keep, h)
		}
	}
	j.held = nil
	if len(keep) == 0 {
		return nil
	}
	if err := os.MkdirAll(j.opt.FailedCalls, 0o700); err != nil {
		return err
	}
	for _, h := range keep {
		if err := os.WriteFile(filepath.Join(j.opt.FailedCalls, fmt.Sprintf("%06d.json", h.id)), h.data, 0o600); err != nil {
			return err
		}
	}
	fmt.Fprintf(j.opt.Log, "kept %d failed model call(s) in %s, until the change is approved\n", len(keep), j.opt.FailedCalls)
	return nil
}

// KeepCalls reports whether a developer asked, with STELE_KEEP_CALLS=1,
// to keep the record of model calls: every request and reply in full
// (requests/), one line per call (requests.jsonl), and the working folders
// of finished checks. Off by default: a project keeps no copy of its
// requirements' prompts, the model's replies or the code names in them,
// only what its checks need (the owner, 2026-11-11).
func KeepCalls() bool {
	v := os.Getenv("STELE_KEEP_CALLS")
	return v != "" && v != "0" && v != "false"
}

func newCallJournal(m llm.Client, dir string, opt Options) (*callJournal, error) {
	if opt.Log == nil {
		opt.Log = io.Discard
	}
	// The ledger (requests.jsonl) holds counts and usage, no prompt text;
	// a call budget across resumed runs (MaxCalls, the research commands)
	// needs it. The full requests and replies need KeepCalls.
	if !KeepCalls() && opt.MaxCalls == 0 {
		j := &callJournal{Client: m, opt: opt, dir: dir}
		j.done = sync.NewCond(&j.mu)
		return j, nil
	}
	f, err := os.OpenFile(filepath.Join(dir, "requests.jsonl"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if KeepCalls() {
		if err := os.MkdirAll(filepath.Join(dir, "requests"), 0o700); err != nil {
			f.Close()
			return nil, err
		}
	}
	j := &callJournal{Client: m, file: f, opt: opt, dir: dir}
	j.done = sync.NewCond(&j.mu)
	dec := json.NewDecoder(f)
	for {
		var e callEvent
		if err := dec.Decode(&e); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			f.Close()
			return nil, fmt.Errorf("read request journal: %w", err)
		}
		switch e.Event {
		case "started":
			j.counts.Started++
		case "completed":
			j.counts.Completed++
		case "failed":
			j.counts.Failed++
		}
	}
	return j, nil
}

func (j *callJournal) close() {
	if j.file != nil {
		j.file.Close()
	}
}

// CachesPrompts is the journaled backend's (llm.Warm).
func (j *callJournal) CachesPrompts() bool { return llm.CachesPrompts(j.Client) }

func (j *callJournal) stats() CallStats {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := j.counts
	s.Interrupted = s.Started - s.Completed - s.Failed
	return s
}

func (j *callJournal) stopErr() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stop
}

// record is called with mu held. A request never starts before its journal
// entry reaches disk, even for backends that cannot report token usage.
func (j *callJournal) record(e callEvent) error {
	if j.file == nil {
		return nil // nothing kept (KeepCalls)
	}
	if err := json.NewEncoder(j.file).Encode(e); err != nil {
		return err
	}
	return j.file.Sync()
}

// reserve is called with mu held. It waits until a call of this many prompt
// bytes fits the cost budget with the calls in flight, and counts it in
// flight; it fails once the call cannot fit even with none in flight.
// Before any call has reported its cost, one call runs at a time, so the
// first call is the only one that can pass the budget.
func (j *callJournal) reserve(bytes int) (float64, error) {
	for {
		if j.stop != nil {
			return 0, j.stop
		}
		expect, known := 0.0, j.pricedBytes > 0
		if known {
			expect = j.pricedCost / j.pricedBytes * float64(bytes)
		}
		if j.opt.MaxCost <= 0 || (j.unpriced && !known) || (j.inflight == 0 && j.spent+expect < j.opt.MaxCost) ||
			(known && j.spent+j.pending+expect <= j.opt.MaxCost) {
			j.inflight++
			j.pending += expect
			return expect, nil
		}
		if j.inflight == 0 {
			break
		}
		j.done.Wait()
	}
	j.stop = fmt.Errorf("cost budget reached ($%.2f of $%.2f); accepted work is saved", j.spent, j.opt.MaxCost)
	return 0, j.stop
}

func (j *callJournal) Complete(ctx context.Context, req llm.Request) (*llm.Reply, error) {
	j.mu.Lock()
	if j.stop != nil {
		err := j.stop
		j.mu.Unlock()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		j.stop = err
		j.mu.Unlock()
		return nil, err
	}
	bytes := len(req.System) + len(req.Schema)
	for _, t := range req.Turns {
		bytes += len(t.Text)
	}
	expect, err := j.reserve(bytes)
	if err != nil {
		j.mu.Unlock()
		return nil, err
	}
	defer func() {
		j.mu.Lock()
		j.inflight--
		j.pending -= expect
		j.done.Broadcast()
		j.mu.Unlock()
	}()
	if j.opt.MaxCalls > 0 && j.counts.Started >= j.opt.MaxCalls {
		err := fmt.Errorf("model-call budget reached (%d); accepted work is saved", j.opt.MaxCalls)
		j.stop = err
		j.mu.Unlock()
		return nil, err
	}
	start := time.Now()
	e := callEvent{ID: j.counts.Started + 1, Event: "started", At: start.UTC(), Backend: j.Name(), PromptBytes: len(req.System), SchemaBytes: len(req.Schema)}
	for _, t := range req.Turns {
		e.PromptBytes += len(t.Text)
	}
	if err := j.record(e); err != nil {
		j.stop = err
		j.mu.Unlock()
		return nil, err
	}
	j.counts.Started++
	fmt.Fprintf(j.opt.Log, "  model call %d: %d prompt bytes, %d schema bytes\n", e.ID, e.PromptBytes, e.SchemaBytes)
	j.mu.Unlock()
	run := ctx // the run's own context, before this call's deadline
	call := func(ctx context.Context) (*llm.Reply, error) {
		if j.opt.RequestTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, j.opt.RequestTimeout)
			defer cancel()
		}
		reply, err := j.Client.Complete(ctx, req)
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		return reply, err
	}
	var reply *llm.Reply
	if h, ok := j.Client.(llm.Hedger); ok {
		// Each attempt has the whole request limit: sharing one, the
		// second attempt of a stalled Codex call had 80 seconds and timed
		// out too (a developer trial, 2026-11-05).
		reply, err = llm.Hedge(run, h.HedgeAfter(), call)
	} else {
		reply, err = call(run)
	}
	if err == nil && reply == nil {
		err = fmt.Errorf("backend returned no reply")
	}
	e.Event, e.At, e.Milliseconds = "completed", time.Now().UTC(), time.Since(start).Milliseconds()
	if reply != nil {
		e.ReplyBytes, e.Usage = len(reply.JSON), reply.Usage
	}
	j.mu.Lock()
	if reply != nil {
		j.spent += reply.Usage.CostUSD
		if reply.Usage.CostUSD > 0 {
			j.pricedBytes += float64(bytes)
			j.pricedCost += reply.Usage.CostUSD
		} else if err == nil {
			j.unpriced = true
		}
	}
	j.mu.Unlock()
	if err != nil {
		e.Event, e.Error = "failed", err.Error()
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	// Cancellation or the run's deadline stops later calls; this call's own
	// deadline fails only this call.
	if run.Err() != nil || errors.Is(err, llm.ErrSandboxed) {
		j.stop = err // a sandboxed call: every later one fails the same way
	}
	// Preserve the exact input and raw reply before callers validate it. This
	// makes failed translations reproducible without another backend call.
	var archiveErr error
	if !KeepCalls() && j.opt.FailedCalls != "" {
		// Held in memory; only a failure's calls reach the disk.
		data, merr := json.MarshalIndent(struct {
			Request llm.Request `json:"request"`
			Reply   *llm.Reply  `json:"reply"`
			Error   string      `json:"error,omitempty"`
		}{req, reply, e.Error}, "", "  ")
		if merr == nil {
			var text strings.Builder
			text.WriteString(req.System)
			for _, t := range req.Turns {
				text.WriteString("\n")
				text.WriteString(t.Text)
			}
			j.held = append(j.held, heldCall{id: e.ID, failed: err != nil && run.Err() == nil, text: text.String(), data: append(data, '\n')})
		}
	}
	if j.file != nil && KeepCalls() {
		e.Artifact = filepath.Join("requests", fmt.Sprintf("%06d.json", e.ID))
		var artifact []byte
		artifact, archiveErr = json.MarshalIndent(struct {
			Request llm.Request `json:"request"`
			Reply   *llm.Reply  `json:"reply"`
			Error   string      `json:"error,omitempty"`
		}{req, reply, e.Error}, "", "  ")
		if archiveErr == nil {
			archiveErr = os.WriteFile(filepath.Join(j.dir, e.Artifact), append(artifact, '\n'), 0o600)
		}
	}
	if archiveErr != nil {
		j.stop = fmt.Errorf("save request artifact: %w", archiveErr)
		return nil, j.stop
	}
	if saveErr := j.record(e); saveErr != nil {
		j.stop = fmt.Errorf("save request outcome: %w", saveErr)
		return nil, j.stop
	}
	if err != nil {
		j.counts.Failed++
	} else {
		j.counts.Completed++
	}
	fmt.Fprintf(j.opt.Log, "  model call %d: %s in %.1fs, %d reply bytes\n", e.ID, e.Event, float64(e.Milliseconds)/1000, e.ReplyBytes)
	return reply, err
}
