package change

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/provasign/stele/internal/llm"
)

// editing is a model client that edits the change's spec while it answers:
// someone saving the file during a check.
type editing struct {
	fake
	once *sync.Once
	edit func()
}

func (e editing) Complete(ctx context.Context, req llm.Request) (*llm.Reply, error) {
	e.once.Do(e.edit)
	return e.fake.Complete(ctx, req)
}

// TestSpecEditedDuringCheck: a requirement edited while the check runs was
// read as it stood before, so the check refuses its own result instead of
// fingerprinting the new text under the old reading (a code review,
// 2026-11-11). Run again, the check reads the new text.
func TestSpecEditedDuringCheck(t *testing.T) {
	repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Erasure\nErasure requests MUST always delete the resource.\n")
	spec := filepath.Join(repo, "openspec/changes/c1/specs/res/spec.md")
	out := t.TempDir()
	f := one("Erasure", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "request.kind = erasure"})
	client := editing{fake: f, once: &sync.Once{}, edit: func() {
		os.WriteFile(spec, []byte("## ADDED Requirements\n\n### Requirement: Erasure\nErasure requests MUST never delete the resource.\n"), 0o644)
	}}
	_, err := Run(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: out, Clients: []llm.Client{client}})
	if !errors.Is(err, ErrChangedDuringCheck) {
		t.Fatalf("a spec edited during the check: err = %v", err)
	}
	res, err := Run(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: out, Clients: []llm.Client{f}})
	if err != nil {
		t.Fatal(err)
	}
	now, _ := Fingerprint(res.Watched...)
	if res.Fingerprint != now || res.HeadFingerprint == "" || res.Statements["res/Erasure"] == "" {
		t.Fatalf("second run: fingerprint %q now %q head %q", res.Fingerprint, now, res.HeadFingerprint)
	}
}
