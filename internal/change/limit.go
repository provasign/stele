package change

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/provasign/stele/internal/llm"
)

// ErrOverBudget: the check's spending limit is reached, so no further model
// call starts.
var ErrOverBudget = errors.New("the spending limit of this check is reached")

// limited puts every model call of one check under its two limits: the
// time one call may take, and what the whole check may spend. The review
// of the texts, the audit, the code reading and the translation each kept
// their own account, or none: a $0.01 limit let two review calls spend
// $0.20, and the translation after them started from the whole budget
// again (a code review, 2026-11-11). A call's cost is known only when it
// answers, so calls already in flight can pass the limit by their own cost;
// no call starts once it is reached.
type limited struct {
	llm.Client
	timeout time.Duration
	max     float64
	mu      sync.Mutex
	spent   float64
	file    string // SpendFile: what earlier runs of the check spent
}

func (l *limited) Complete(ctx context.Context, req llm.Request) (*llm.Reply, error) {
	l.mu.Lock()
	over := l.max > 0 && l.spent >= l.max
	l.mu.Unlock()
	if over {
		return nil, ErrOverBudget
	}
	if l.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.timeout)
		defer cancel()
	}
	reply, err := l.Client.Complete(ctx, req)
	if reply != nil {
		l.mu.Lock()
		l.spent += reply.Usage.CostUSD
		if l.file != "" {
			_ = os.WriteFile(l.file, []byte(strconv.FormatFloat(l.spent, 'f', -1, 64)+"\n"), 0o600)
		}
		l.mu.Unlock()
	}
	return reply, err
}

// CachesPrompts is the limited backend's (llm.Warm).
func (l *limited) CachesPrompts() bool { return llm.CachesPrompts(l.Client) }

// left is what the check may still spend; 0 when it has no limit.
func (l *limited) left() float64 {
	if l.max <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// A limit of nothing left still is a limit: 0 would mean none.
	return max(l.max-l.spent, 0.000001)
}

// limitedHedger is limited for a backend whose stalled calls are sent again.
type limitedHedger struct {
	*limited
	h llm.Hedger
}

func (l limitedHedger) HedgeAfter() time.Duration { return l.h.HedgeAfter() }

// limiter is the check's limited client, if opt has one.
func limiter(opt Options) *limited {
	if len(opt.Clients) == 0 {
		return nil
	}
	switch c := opt.Clients[0].(type) {
	case *limited:
		return c
	case limitedHedger:
		return c.limited
	}
	return nil
}

// withLimits puts opt's first client under the check's limits, once: a
// check that runs again within itself keeps the account it has.
func withLimits(opt Options) Options {
	if len(opt.Clients) == 0 || limiter(opt) != nil {
		return opt
	}
	l := &limited{Client: opt.Clients[0], timeout: opt.RequestTimeout, max: opt.MaxCost, file: opt.SpendFile}
	if opt.SpendFile != "" {
		if b, err := os.ReadFile(opt.SpendFile); err == nil {
			l.spent, _ = strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		}
	}
	clients := append([]llm.Client{}, opt.Clients...)
	clients[0] = l
	if h, ok := opt.Clients[0].(llm.Hedger); ok {
		clients[0] = limitedHedger{l, h}
	}
	opt.Clients = clients
	return opt
}
