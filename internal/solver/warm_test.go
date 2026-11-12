package solver

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func needZ3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
}

// TestWarmAgrees: a warm z3 answers each script as a new process does,
// including models, cores, the error a get-value gives after unsat, and
// declarations an earlier script made (reset clears them).
func TestWarmAgrees(t *testing.T) {
	needZ3(t)
	scripts := []string{
		"(set-option :produce-models true)\n(declare-const x Int)\n(assert (> x 41))\n(assert (< x 43))\n(check-sat)\n(get-value (x))\n",
		"(declare-const x Bool)\n(assert x)\n(assert (not x))\n(check-sat)\n(get-value (x))\n",
		"(set-option :produce-unsat-cores true)\n(declare-const y Int)\n(assert (! (> y 0) :named a))\n(assert (! (< y 0) :named b))\n(check-sat)\n(get-unsat-core)\n",
		// x is declared again: it would be an error had reset not cleared it.
		"(declare-const x Int)\n(assert (= x 7))\n(check-sat)\n",
		"(declare-datatypes ((C 0)) (((red) (green))))\n(declare-const c C)\n(assert (not (= c red)))\n(check-sat)\n",
	}
	cold, warm := &Z3{Path: "z3"}, &Z3{Path: "z3", Warm: true}
	for round := range 2 {
		for _, sc := range scripts {
			a, aerr := cold.Run(context.Background(), sc)
			b, berr := warm.Run(context.Background(), sc)
			if (aerr != nil) != (berr != nil) || (aerr == nil && !reflect.DeepEqual(a, b)) {
				t.Errorf("round %d, %q:\ncold %+v %v\nwarm %+v %v", round, sc, a, aerr, b, berr)
			}
		}
	}
}

// TestSolversShareWarmProcesses: each check makes a solver of its own;
// their idle processes are shared, not left running one set per solver (a
// code review, 2026-11-04).
func TestSolversShareWarmProcesses(t *testing.T) {
	needZ3(t)
	before := max(idleCount("z3"), 1)
	for range 3 {
		z := &Z3{Path: "z3", Warm: true}
		if r, err := z.Run(context.Background(), "(declare-const x Int)\n(assert (> x 0))\n(check-sat)\n"); err != nil || r.Status != Sat {
			t.Fatalf("%+v %v", r, err)
		}
	}
	if n := idleCount("z3"); n != before {
		t.Fatalf("%d idle z3 processes after three solvers ran one script each, want %d", n, before)
	}
}

// TestWarmCancel: a caller that gives up gets unknown, and the next script
// still runs.
func TestWarmCancel(t *testing.T) {
	needZ3(t)
	z := &Z3{Path: "z3", Warm: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := z.Run(ctx, "(declare-const x Int)\n(assert (> x 0))\n(check-sat)\n"); err != nil || r.Status != Unknown {
		t.Fatalf("cancelled: %+v %v, want unknown", r, err)
	}
	if r, err := z.Run(context.Background(), "(declare-const x Int)\n(assert (> x 0))\n(check-sat)\n"); err != nil || r.Status != Sat {
		t.Fatalf("after cancel: %+v %v, want sat", r, err)
	}
}

// TestWarmError: a script the solver rejects is an error, and the next
// script still runs.
func TestWarmError(t *testing.T) {
	needZ3(t)
	z := &Z3{Path: "z3", Warm: true, Timeout: 2 * time.Second}
	if _, err := z.Run(context.Background(), "(assert undeclared)\n(check-sat)\n"); err == nil || !strings.Contains(err.Error(), "solver error") {
		t.Fatalf("got %v, want a solver error", err)
	}
	if r, err := z.Run(context.Background(), "(check-sat)\n"); err != nil || r.Status != Sat {
		t.Fatalf("after an error: %+v %v, want sat", r, err)
	}
}
