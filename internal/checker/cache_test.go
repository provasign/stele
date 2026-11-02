package checker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
)

func lowered(t *testing.T, yaml string) *lower.Program {
	t.Helper()
	reg, err := ir.LoadDir(writeRegistry(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	prog, err := lower.Lower(reg, lower.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return prog
}

// TestPersistedCache: a cache in a directory answers a later check, as a
// later process would, with the same result; a damaged entry is a miss; a
// changed rule re-solves only the units that hold it.
func TestPersistedCache(t *testing.T) {
	z := needZ3(t)
	dir := t.TempDir()
	ctx := context.Background()
	prog := lowered(t, resources)
	first := &Cache{Dir: dir}
	want, err := CheckWith(ctx, z, prog, Options{Cache: first})
	if err != nil {
		t.Fatal(err)
	}
	if first.Hits != 0 || first.Misses == 0 {
		t.Fatalf("first run: hits %d misses %d", first.Hits, first.Misses)
	}

	later := &Cache{Dir: dir}
	got, err := CheckWith(ctx, z, prog, Options{Cache: later})
	if err != nil {
		t.Fatal(err)
	}
	if later.Misses != 0 || later.Hits != first.Misses {
		t.Errorf("later run: hits %d misses %d, want %d hits", later.Hits, later.Misses, first.Misses)
	}
	if !reflect.DeepEqual(cores(got), cores(want)) || got.Verdict != want.Verdict {
		t.Errorf("from the cache %s %q, solved %s %q", got.Verdict, cores(got), want.Verdict, cores(want))
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != first.Misses {
		t.Fatalf("%d files for %d units", len(files), first.Misses)
	}
	for _, f := range files {
		os.WriteFile(f, []byte("{not json"), 0o644)
	}
	damaged := &Cache{Dir: dir}
	got, err = CheckWith(ctx, z, prog, Options{Cache: damaged})
	if err != nil {
		t.Fatal(err)
	}
	if damaged.Hits != 0 || !reflect.DeepEqual(cores(got), cores(want)) {
		t.Errorf("damaged entries: hits %d, cores %q", damaged.Hits, cores(got))
	}

	changed := lowered(t, strings.Replace(resources, "when: self.activeHolds > 0", "when: self.activeHolds > 1", 1))
	again := &Cache{Dir: dir}
	if _, err := CheckWith(ctx, z, changed, Options{Cache: again}); err != nil {
		t.Fatal(err)
	}
	if again.Misses != 1 || again.Hits != first.Misses-1 {
		t.Errorf("one changed rule: hits %d misses %d, want one miss", again.Hits, again.Misses)
	}
}
