package change

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFingerprintShale: commands Shale records between the check and the
// approval leave the fingerprint alone; a new request changes it.
func TestFingerprintShale(t *testing.T) {
	repo := t.TempDir()
	local := filepath.Join(repo, ".shale", "local")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(local, "s1.jsonl")
	appendLine := func(line string) {
		f, err := os.OpenFile(session, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(line + "\n")
		f.Close()
	}
	appendLine(`{"kind":"intent","at":"2026-10-01T09:00:00Z","title":"Orders ship within 2 days"}`)
	if err := keepAll(repo); err != nil {
		t.Fatal(err)
	}
	before, err := Fingerprint(filepath.Join(repo, ".shale"))
	if err != nil {
		t.Fatal(err)
	}
	appendLine(`{"kind":"command","at":"2026-10-01T09:01:00Z","cmd":"go test ./..."}`)
	if after, _ := Fingerprint(filepath.Join(repo, ".shale")); after != before {
		t.Errorf("a recorded command changed the fingerprint")
	}
	appendLine(`{"kind":"intent","at":"2026-10-01T09:02:00Z","title":"Orders ship within 3 days"}`)
	if after, _ := Fingerprint(filepath.Join(repo, ".shale")); after == before {
		t.Errorf("a restated request left the fingerprint unchanged")
	}
}
