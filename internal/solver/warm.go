package solver

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// A pool holds idle z3 processes in interactive mode.
type pool struct {
	path string
	mu   sync.Mutex
	idle []*session
}

type session struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan string // stdout, line by line; closed when z3 exits
	n     int
}

// pools holds one pool per z3 binary for the whole process. Each solver
// had a pool of its own, and nothing stopped its processes when the solver
// was dropped: every check stele serve ran left its z3 children running (a
// code review, 2026-11-04). Shared, the idle processes stay at one per CPU
// however many solvers a process makes, and a new solver starts warm.
var pools sync.Map // path -> *pool

func (z *Z3) pool() *pool {
	z.once.Do(func() {
		if z.sessions == nil {
			p, _ := pools.LoadOrStore(z.path(), &pool{path: z.path()})
			z.sessions = p.(*pool)
		}
	})
	return z.sessions
}

// idleCount is the number of idle processes kept for path.
func idleCount(path string) int {
	p, ok := pools.Load(path)
	if !ok {
		return 0
	}
	p.(*pool).mu.Lock()
	defer p.(*pool).mu.Unlock()
	return len(p.(*pool).idle)
}

func (p *pool) get() (*session, error) {
	p.mu.Lock()
	if n := len(p.idle); n > 0 {
		s := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return s, nil
	}
	p.mu.Unlock()
	cmd := exec.Command(p.path, "-in", "-smt2")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cannot run %s: %w (install with `brew install z3`)", p.path, err)
	}
	s := &session{cmd: cmd, in: in, lines: make(chan string, 64)}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<16), 1<<26)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
		close(s.lines)
		cmd.Wait()
	}()
	return s, nil
}

// put keeps a session for the next script, up to one per CPU.
func (p *pool) put(s *session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.idle) < runtime.NumCPU() {
		p.idle = append(p.idle, s)
		return
	}
	s.kill()
}

func (s *session) kill() {
	s.in.Close()
	s.cmd.Process.Kill()
}

// runWarm sends script to a running z3 after (reset), which clears every
// declaration and assertion, so the answer is the one a new process gives.
// z3's own timeout bounds each check; a z3 that does not answer in time, or
// whose caller gives up, is killed, and the answer is unknown.
func (z *Z3) runWarm(ctx context.Context, script string, timeout time.Duration) (*Result, error) {
	p := z.pool()
	s, err := p.get()
	if err != nil {
		return nil, err
	}
	s.n++
	mark := fmt.Sprintf("stele-done-%d", s.n)
	payload := fmt.Sprintf("(reset)\n(set-option :timeout %d)\n%s\n(echo \"%s\")\n", timeout.Milliseconds(), script, mark)
	go io.WriteString(s.in, payload) // z3 answers while it reads; a full pipe must not block the reader
	timer := time.NewTimer(timeout + 2*time.Second)
	defer timer.Stop()
	start := time.Now()
	var out strings.Builder
	for done := false; !done; {
		select {
		case line, ok := <-s.lines:
			if !ok {
				return nil, fmt.Errorf("solver exited without an answer: %s", out.String())
			}
			if strings.Trim(strings.TrimSpace(line), `"`) == mark {
				done = true
				break
			}
			out.WriteString(line)
			out.WriteByte('\n')
		case <-timer.C:
			s.kill()
			return &Result{Status: Unknown}, nil
		case <-ctx.Done():
			s.kill()
			return &Result{Status: Unknown}, nil
		}
	}
	res, err := z.answer(script, out.String(), time.Since(start), timeout)
	if err != nil {
		s.kill()
		return nil, err
	}
	p.put(s)
	if res == nil {
		return nil, fmt.Errorf("solver gave no answer: %s", out.String())
	}
	return res, nil
}
