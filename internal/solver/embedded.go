//go:build z3static

// Stele's release binaries carry Z3 inside them, linked from a static libz3
// (scripts/build-z3.sh), as Prism carries its engine: a user installs one
// file and nothing else. A build without the z3static tag runs the z3 on
// PATH instead.

package solver

/*
#cgo LDFLAGS: -lz3
#cgo linux LDFLAGS: -lstdc++ -lm -lpthread
#cgo darwin LDFLAGS: -lc++
#include <stdlib.h>
#include <z3.h>

static Z3_context stele_ctx(void) {
	Z3_config cfg = Z3_mk_config();
	Z3_context c = Z3_mk_context(cfg);
	Z3_del_config(cfg);
	Z3_set_error_handler(c, NULL); // errors come back as codes, never exit the process
	return c;
}
*/
import "C"

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

func init() {
	var major, minor, build, rev C.uint
	C.Z3_get_version(&major, &minor, &build, &rev)
	builtin = &inProcess{version: fmt.Sprintf("Z3 version %d.%d.%d - 64 bit", major, minor, build)}
}

// inProcess runs scripts in Z3 contexts inside this process. Each context
// serves one script at a time; idle ones are kept, up to one per CPU, and
// cleared with (reset) before the next script, as a warm z3 process is.
type inProcess struct {
	version string
	mu      sync.Mutex
	idle    []C.Z3_context
}

func (e *inProcess) Version() string { return e.version }

func (e *inProcess) get() C.Z3_context {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n := len(e.idle); n > 0 {
		c := e.idle[n-1]
		e.idle = e.idle[:n-1]
		return c
	}
	return C.stele_ctx()
}

func (e *inProcess) put(c C.Z3_context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.idle) < runtime.NumCPU() {
		e.idle = append(e.idle, c)
		return
	}
	C.Z3_del_context(c)
}

// Eval runs script and returns what z3 prints for it. Z3's own timeout
// bounds each check; past timeout plus two seconds, or when ctx ends, the
// context is interrupted, and interrupted reports so.
func (e *inProcess) Eval(ctx context.Context, script string, timeout time.Duration) (out string, interrupted bool, err error) {
	c := e.get()
	payload := fmt.Sprintf("(reset)\n(set-option :timeout %d)\n%s\n", timeout.Milliseconds(), script)
	cs := C.CString(payload)
	defer C.free(unsafe.Pointer(cs))

	done := make(chan struct{})
	var stopped sync.Once
	var wasStopped bool
	go func() {
		timer := time.NewTimer(timeout + 2*time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			stopped.Do(func() { wasStopped = true; C.Z3_interrupt(c) })
		case <-ctx.Done():
			stopped.Do(func() { wasStopped = true; C.Z3_interrupt(c) })
		}
	}()
	r := C.Z3_eval_smtlib2_string(c, cs)
	code := C.Z3_get_error_code(c)
	if r != nil {
		out = C.GoString(r) // copied before the context is used again
	}
	close(done)
	stopped.Do(func() {}) // the watcher can no longer interrupt this context
	if code != C.Z3_OK {
		err = fmt.Errorf("solver error: %s", C.GoString(C.Z3_get_error_msg(c, code)))
		C.Z3_del_context(c) // a context left in error is not reused
		return out, wasStopped, err
	}
	if wasStopped {
		C.Z3_del_context(c)
		return out, true, nil
	}
	e.put(c)
	return out, false, nil
}
