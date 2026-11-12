//go:build z3static

package solver

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Z3 built into the binary answers as a z3 process does, with no z3 on
// PATH.
func TestBuiltinAnswers(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("STELE_Z3", "")
	z := &Z3{}
	if v, err := z.Version(); err != nil || !strings.HasPrefix(v, "Z3 version ") {
		t.Fatalf("version %q, %v", v, err)
	}
	ctx := context.Background()
	res, err := z.Run(ctx, "(declare-const x Int)\n(assert (> x 2))\n(check-sat)\n(get-value (x))\n")
	if err != nil || res.Status != Sat || len(res.Replies) != 1 {
		t.Fatalf("sat: %+v, %v", res, err)
	}
	res, err = z.Run(ctx, "(set-option :produce-unsat-cores true)\n(declare-const x Int)\n(assert (! (> x 2) :named a))\n(assert (! (< x 1) :named b))\n(check-sat)\n(get-unsat-core)\n")
	if err != nil || res.Status != Unsat || len(res.Replies) != 1 || len(res.Replies[0].List) != 2 {
		t.Fatalf("unsat core: %+v, %v", res, err)
	}
	// A script after another starts clean: x is undeclared again.
	res, err = z.Run(ctx, "(declare-const x Bool)\n(assert x)\n(check-sat)\n")
	if err != nil || res.Status != Sat {
		t.Fatalf("after reset: %+v, %v", res, err)
	}
	// get-value after unsat errors by design, and is ignored.
	res, err = z.Run(ctx, "(declare-const x Int)\n(assert (and (> x 2) (< x 1)))\n(check-sat)\n(get-value (x))\n")
	if err != nil || res.Status != Unsat {
		t.Fatalf("get-value after unsat: %+v, %v", res, err)
	}
	if _, err := z.Run(ctx, "(assert y)\n(check-sat)\n"); err == nil {
		t.Fatal("an undeclared symbol gave no error")
	}
	// The process survives the error, and the next script is answered.
	if res, err := z.Run(ctx, "(check-sat)\n"); err != nil || res.Status != Sat {
		t.Fatalf("after an error: %+v, %v", res, err)
	}
}

// A caller that gives up gets unknown, and the solver stays usable.
func TestBuiltinCancel(t *testing.T) {
	z := &Z3{Timeout: time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := z.Run(ctx, hardScript)
	if err != nil || res.Status != Unknown {
		t.Fatalf("cancelled: %+v, %v", res, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("cancel took %v", d)
	}
	if res, err := z.Run(context.Background(), "(check-sat)\n"); err != nil || res.Status != Sat {
		t.Fatalf("after cancel: %+v, %v", res, err)
	}
}

// Z3's own timeout gives unknown.
func TestBuiltinTimeout(t *testing.T) {
	z := &Z3{Timeout: 300 * time.Millisecond}
	res, err := z.Run(context.Background(), hardScript)
	if err != nil || res.Status != Unknown {
		t.Fatalf("timeout: %+v, %v", res, err)
	}
}

// Scripts run at once each get their own answer.
func TestBuiltinConcurrent(t *testing.T) {
	z := &Z3{}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want, script := Sat, "(declare-const x Int)\n(assert (> x 2))\n(check-sat)\n"
			if i%2 == 1 {
				want, script = Unsat, "(declare-const x Int)\n(assert (and (> x 2) (< x 1)))\n(check-sat)\n"
			}
			res, err := z.Run(context.Background(), script)
			if err != nil || res.Status != want {
				errs <- "wrong answer"
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

// hardScript is a nonlinear problem z3 takes far longer than a second on.
const hardScript = `(declare-const a Int)
(declare-const b Int)
(declare-const c Int)
(assert (> a 1))
(assert (> b 1))
(assert (> c 1))
(assert (= (+ (* a a a) (* b b b)) (* c c c)))
(check-sat)
`
