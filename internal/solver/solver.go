// Package solver drives an SMT-LIB 2 solver as a subprocess (D-034). A
// timeout is reported as Unknown, never as a pass (docs/06-checker.md).
package solver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Status is a check-sat answer.
type Status string

const (
	Sat     Status = "sat"
	Unsat   Status = "unsat"
	Unknown Status = "unknown"
)

// Z3 runs scripts through a z3 binary.
type Z3 struct {
	Path    string        // defaults to "z3" on PATH
	Timeout time.Duration // per script; defaults to 10 s
	// Warm keeps z3 processes running between scripts and clears each with
	// (reset) before the next, instead of starting a process per script.
	Warm bool

	nearLimit atomic.Int64
	sessions  *pool
	once      sync.Once
}

// DefaultWarm makes every solver warm. The stele binary sets it; library
// callers and tests start a process per script unless they ask.
var DefaultWarm bool

// Fork returns a solver with the same settings and warm processes, and its
// own NearLimit count.
func (z *Z3) Fork() *Z3 {
	return &Z3{Path: z.Path, Timeout: z.Timeout, Warm: z.Warm, sessions: z.pool()}
}

// NearLimit counts the definite answers so far that took at least 80% of the
// timeout. Such an answer is still exact, but a slower machine would have
// given unknown in its place: the verdict depends on machine speed.
func (z *Z3) NearLimit() int64 { return z.nearLimit.Load() }

// Version returns the solver's version line, e.g. "Z3 version 5.3.1 - 64 bit".
// It is remembered per binary (path, size and modification time), so a
// long-running process asks once.
func (z *Z3) Version() (string, error) {
	key := ""
	if bin, err := exec.LookPath(z.path()); err == nil {
		if fi, err := os.Stat(bin); err == nil {
			key = fmt.Sprintf("%s %d %d", bin, fi.Size(), fi.ModTime().UnixNano())
			if v, ok := versions.Load(key); ok {
				return v.(string), nil
			}
		}
	}
	out, err := exec.Command(z.path(), "--version").Output()
	if err != nil {
		return "", fmt.Errorf("cannot run %s: %w (Stele needs the Z3 solver; install it with `%s`)", z.path(), err, z3Install())
	}
	v := strings.TrimSpace(string(out))
	if key != "" {
		versions.Store(key, v)
	}
	return v, nil
}

var versions sync.Map // binary key -> version line

// z3Install is the command that installs Z3 on this system.
func z3Install() string {
	switch runtime.GOOS {
	case "darwin":
		return "brew install z3"
	case "windows":
		return "winget install z3"
	}
	for _, pm := range []struct{ bin, cmd string }{{"apt-get", "sudo apt-get install z3"}, {"dnf", "sudo dnf install z3"}, {"pacman", "sudo pacman -S z3"}, {"apk", "sudo apk add z3"}, {"brew", "brew install z3"}} {
		if _, err := exec.LookPath(pm.bin); err == nil {
			return pm.cmd
		}
	}
	return "your package manager's z3 package, or https://github.com/Z3Prover/z3/releases"
}

func (z *Z3) path() string {
	if z.Path == "" {
		return "z3"
	}
	return z.Path
}

// Result is the status of the script's one check-sat, plus the responses to
// any commands after it (get-value, get-unsat-core).
type Result struct {
	Status  Status
	Replies []SExpr
}

// Run sends script to the solver. The script must contain exactly one check-sat.
func (z *Z3) Run(ctx context.Context, script string) (*Result, error) {
	timeout := z.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	if z.Warm || DefaultWarm {
		return z.runWarm(ctx, script, timeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+2*time.Second)
	defer cancel()
	// -T is z3's own wall-clock limit; the context is the backstop.
	cmd := exec.CommandContext(ctx, z.path(), "-in", "-smt2", fmt.Sprintf("-T:%d", int(timeout.Seconds()+0.999)))
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	if ctx.Err() != nil {
		return &Result{Status: Unknown}, nil
	}
	res, perr := z.answer(script, stdout.String(), time.Since(start), timeout)
	if perr != nil || res != nil {
		return res, perr
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && strings.Contains(stdout.String()+stderr.String(), "timeout") {
		return &Result{Status: Unknown}, nil
	}
	return nil, fmt.Errorf("solver gave no answer (%v): %s%s", err, stdout.String(), stderr.String())
}

// answer reads the solver's output for script. It returns nil and no error
// when the output holds no check-sat answer.
func (z *Z3) answer(script, out string, elapsed, timeout time.Duration) (*Result, error) {
	exprs, perr := Parse(out)
	if perr != nil {
		return nil, fmt.Errorf("unreadable solver output: %v\n%s", perr, out)
	}
	var res *Result
	for _, e := range exprs {
		if e.IsList() && len(e.List) == 2 && e.List[0].Atom == "error" {
			if res != nil && res.Status != Sat {
				continue // get-value after unsat or unknown errors by design; ignore it
			}
			return nil, fmt.Errorf("solver error: %s\n--- script ---\n%s", e.List[1].Atom, script)
		}
		if res == nil {
			switch Status(e.Atom) {
			case Sat, Unsat, Unknown:
				res = &Result{Status: Status(e.Atom)}
			case "timeout":
				res = &Result{Status: Unknown}
			}
			continue
		}
		res.Replies = append(res.Replies, e)
	}
	if res != nil && res.Status == Unknown {
		dumpUnknown(script, out)
	}
	if res != nil && res.Status != Unknown && elapsed*5 >= timeout*4 {
		z.nearLimit.Add(1)
	}
	return res, nil
}

// SExpr is an atom or a list.
type SExpr struct {
	Atom string // quoted symbols keep their bars: |x|
	List []SExpr
	// Str marks a string literal; Atom holds its text without quotes.
	Str bool
}

// IsList reports whether e is a list.
func (e SExpr) IsList() bool { return e.List != nil }

func (e SExpr) String() string {
	if e.Str {
		return `"` + strings.ReplaceAll(e.Atom, `"`, `""`) + `"`
	}
	if !e.IsList() {
		return e.Atom
	}
	parts := make([]string, len(e.List))
	for i, x := range e.List {
		parts[i] = x.String()
	}
	return "(" + strings.Join(parts, " ") + ")"
}

// Parse reads a sequence of s-expressions.
func Parse(src string) ([]SExpr, error) {
	var stack [][]SExpr
	var top []SExpr
	push := func(e SExpr) {
		if len(stack) == 0 {
			top = append(top, e)
		} else {
			stack[len(stack)-1] = append(stack[len(stack)-1], e)
		}
	}
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\n' || c == '\t' || c == '\r':
			i++
		case c == ';':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '(':
			stack = append(stack, []SExpr{})
			i++
		case c == ')':
			if len(stack) == 0 {
				return nil, errors.New("unbalanced ')'")
			}
			l := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if l == nil {
				l = []SExpr{}
			}
			push(SExpr{List: l})
			i++
		case c == '|':
			j := strings.IndexByte(src[i+1:], c)
			if j < 0 {
				return nil, fmt.Errorf("unterminated %c", c)
			}
			push(SExpr{Atom: src[i : i+j+2]})
			i += j + 2
		case c == '"':
			// A string literal; "" inside it is one quote (SMT-LIB 2.6).
			j := i + 1
			for {
				k := strings.IndexByte(src[j:], '"')
				if k < 0 {
					return nil, errors.New("unterminated string")
				}
				j += k + 1
				if j < len(src) && src[j] == '"' {
					j++
					continue
				}
				break
			}
			push(SExpr{Atom: strings.ReplaceAll(src[i+1:j-1], `""`, `"`), Str: true})
			i = j
		default:
			j := i
			for j < len(src) && !strings.ContainsRune(" \n\t\r()|\";", rune(src[j])) {
				j++
			}
			push(SExpr{Atom: src[i:j]})
			i = j
		}
	}
	if len(stack) > 0 {
		return nil, errors.New("unbalanced '('")
	}
	return top, nil
}

// dumpUnknown saves a script the solver could not decide into the directory
// named by STELE_DUMP_UNKNOWN, for diagnosis.
func dumpUnknown(script, reply string) {
	dir := os.Getenv("STELE_DUMP_UNKNOWN")
	if dir == "" {
		return
	}
	f, err := os.CreateTemp(dir, "unknown-*.smt2")
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\n; reply:\n; %s\n", script, strings.ReplaceAll(reply, "\n", "\n; "))
}
