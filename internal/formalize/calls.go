package formalize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/provasign/stele/internal/llm"
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
	unpriced    bool // a call completed without a cost: the budget cannot apply
	done        *sync.Cond
}

func newCallJournal(m llm.Client, dir string, opt Options) (*callJournal, error) {
	if opt.Log == nil {
		opt.Log = io.Discard
	}
	f, err := os.OpenFile(filepath.Join(dir, "requests.jsonl"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "requests"), 0o700); err != nil {
		f.Close()
		return nil, err
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

func (j *callJournal) close() { j.file.Close() }

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
	if j.opt.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, j.opt.RequestTimeout)
		defer cancel()
	}
	reply, err := j.Client.Complete(ctx, req)
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
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
	if run.Err() != nil {
		j.stop = err
	}
	// Preserve the exact input and raw reply before callers validate it. This
	// makes failed translations reproducible without another backend call.
	e.Artifact = filepath.Join("requests", fmt.Sprintf("%06d.json", e.ID))
	artifact, archiveErr := json.MarshalIndent(struct {
		Request llm.Request `json:"request"`
		Reply   *llm.Reply  `json:"reply"`
		Error   string      `json:"error,omitempty"`
	}{req, reply, e.Error}, "", "  ")
	if archiveErr == nil {
		archiveErr = os.WriteFile(filepath.Join(j.dir, e.Artifact), append(artifact, '\n'), 0o600)
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
