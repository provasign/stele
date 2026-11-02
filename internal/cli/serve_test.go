package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/provasign/stele/internal/checker"
)

const serveRegistry = `
context: orders
formatVersion: 1
---
entity: Order
context: orders
attributes:
  shipped: { type: bool, controller: system }
  express: { type: bool }
---
operation: Order.ship
context: orders
default: allow
writes: [Order.shipped]
---
id: O1
kind: invariant
when: self.express
assert: self.shipped
subject: Order
---
id: O2
kind: invariant
assert: not self.shipped
subject: Order
`

// ask sends one request to answer over a pipe, as a client would.
func ask(t *testing.T, req request) response {
	t.Helper()
	client, server := net.Pipe()
	go answer(server, os.Stderr)
	client.SetDeadline(time.Now().Add(time.Minute))
	if err := json.NewEncoder(client).Encode(req); err != nil {
		t.Fatal(err)
	}
	var resp response
	if err := json.NewDecoder(client).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	client.Close()
	return resp
}

// TestServeAnswersAsLocal: `stele serve` answers `stele check` as the
// command run locally does, before and after an edit to the registry,
// and refuses a client of another build.
func TestServeAnswersAsLocal(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "registry.yaml")
	if err := os.WriteFile(file, []byte(serveRegistry), 0o644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	local := func() (string, string, int) {
		var out, errOut bytes.Buffer
		code := checkCmd([]string{dir}, &out, &errOut)
		return out.String(), errOut.String(), code
	}
	serving = true
	defer func() { serving = false }()
	for _, edit := range []string{"", "assert: not self.shipped\n", "assert: self.shipped or not self.express\n"} {
		if edit != "" {
			src := strings.Replace(serveRegistry, "assert: not self.shipped\n", edit, 1)
			// A new size or time shows the edit, as an editor's save does.
			if err := os.WriteFile(file, []byte(src+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		resp := ask(t, request{Build: checker.BuildID(), Dir: wd, Args: []string{"check", dir}})
		serving = false
		out, errOut, code := local()
		serving = true
		if resp.Refused != "" || resp.Stdout != out || resp.Stderr != errOut || resp.Code != code {
			t.Errorf("edit %q: served %+v\nlocal %q %q %d", edit, resp, out, errOut, code)
		}
	}
	if resp := ask(t, request{Build: "another", Dir: wd, Args: []string{"check", dir}}); resp.Refused == "" {
		t.Errorf("a client of another build was answered: %+v", resp)
	}
}

// TestNoServer: with no server listening, the client runs the command.
func TestNoServer(t *testing.T) {
	t.Setenv("STELE_SOCKET", filepath.Join(t.TempDir(), "none.sock"))
	if _, ok := viaServer([]string{"check", "x"}); ok {
		t.Error("answered with no server")
	}
}
