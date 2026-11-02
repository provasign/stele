package translate

import (
	"os"
	"path/filepath"
	"testing"
)

// An input vocabulary file named like a file of the run's own (a previous
// run's vocabulary.yaml) is copied in under another name, never over it.
func TestWriteRegistryKeepsTheRunsOwnFiles(t *testing.T) {
	in, out := t.TempDir(), t.TempDir()
	input := filepath.Join(in, "vocabulary.yaml")
	own := filepath.Join(out, "vocabulary.yaml")
	if err := os.WriteFile(input, []byte("# input\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, []byte("# proposal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteRegistry(out, &Report{}, nil, []string{input, own}); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{own: "# proposal\n", filepath.Join(out, "input-vocabulary.yaml"): "# input\n"} {
		if got, err := os.ReadFile(f); err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %q", filepath.Base(f), got, err, want)
		}
	}
}
