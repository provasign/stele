package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/specsync"
	"github.com/provasign/stele/internal/translate"
	"gopkg.in/yaml.v3"
)

func TestSavedSpecDiagnosticsThroughStdio(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project with spaces")
	specs := filepath.Join(root, "specs")
	reg := filepath.Join(root, "registry")
	for _, p := range []string{specs, reg} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(specs, "door.md")
	original := "# Door\n\nREQ-1: The door MUST be locked.\n"
	put := func(p, text string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put(file, original)
	stmts, err := importer.Markdown(specs, importer.MarkdownOptions{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(root, "statements.yaml")
	data, _ := yaml.Marshal(stmts)
	put(snapshot, string(data))
	vocab := filepath.Join(reg, "context.yaml")
	put(vocab, "context: c\nformatVersion: 1\n---\nentity: Door\ncontext: c\nattributes:\n  locked: {type: bool}\n")
	if err := translate.WriteRegistry(reg, &translate.Report{Candidates: []translate.Candidate{{StatementID: stmts[0].ID, RuleID: "R1", Outcome: "rule", Kind: "invariant", Assert: "door.locked"}}}, stmts, []string{vocab}); err != nil {
		t.Fatal(err)
	}
	in, client := io.Pipe()
	output, serverOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); client.Close(); in.Close(); output.Close(); serverOut.Close() })
	server := Server{Interval: 10 * time.Millisecond, Resolve: func(workspace string) (specsync.Options, error) {
		if workspace != root {
			t.Errorf("workspace URI decoded incorrectly: %s", workspace)
		}
		return specsync.Options{Root: specs, Format: "markdown", Snapshot: snapshot, Registry: reg}, nil
	}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, in, serverOut) }()
	messages := make(chan message, 64)
	readErrors := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(output)
		for {
			m, err := read(reader)
			if err != nil {
				readErrors <- err
				return
			}
			messages <- m
		}
	}()
	w := writer{out: client}
	send := func(id int, method string, params any) {
		t.Helper()
		m := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
		if id > 0 {
			m["id"] = id
		}
		if err := w.send(m); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(match func(message) bool) message {
		t.Helper()
		deadline := time.After(10 * time.Second) // generous: CI runs the solver-heavy packages alongside
		for {
			select {
			case m := <-messages:
				if match(m) {
					return m
				}
			case err := <-readErrors:
				t.Fatal(err)
			case <-deadline:
				t.Fatal("language server did not respond")
			}
		}
	}
	send(1, "initialize", map[string]string{"rootUri": FileURI(root)})
	init := wait(func(m message) bool { return string(m.ID) == "1" })
	if init.Error != nil {
		t.Fatal(init.Error.Message)
	}
	send(0, "initialized", map[string]any{})
	send(2, "workspace/executeCommand", map[string]string{"command": "stele.sync"})
	m := wait(func(m message) bool { return string(m.ID) == "2" })
	var status specsync.Result
	if err := json.Unmarshal(m.Result, &status); err != nil || !status.InSync {
		t.Fatalf("initial status: %s, %v", m.Result, err)
	}
	diagnostics := func(uri, code string, empty bool) func(message) bool {
		return func(m message) bool {
			if m.Method != "textDocument/publishDiagnostics" {
				return false
			}
			var p struct {
				URI         string `json:"uri"`
				Diagnostics []struct {
					Code  string `json:"code"`
					Range struct {
						Start struct {
							Line int `json:"line"`
						} `json:"start"`
					} `json:"range"`
				} `json:"diagnostics"`
			}
			if err := json.Unmarshal(m.Params, &p); err != nil || p.URI != uri {
				return false
			}
			if empty {
				return len(p.Diagnostics) == 0
			}
			for _, d := range p.Diagnostics {
				if d.Code == code {
					if code == "spec.changed" && d.Range.Start.Line != 2 {
						t.Errorf("diagnostic line=%d, want 2", d.Range.Start.Line)
					}
					return true
				}
			}
			return false
		}
	}
	put(file, "# Door\n\nREQ-1: The door MUST be unlocked.\n")
	send(0, "textDocument/didSave", map[string]any{"textDocument": map[string]string{"uri": FileURI(file)}})
	wait(diagnostics(FileURI(file), "spec.changed", false))
	put(file, original)
	wait(diagnostics(FileURI(file), "", true))
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	wait(diagnostics(FileURI(filepath.Join(reg, "rules.yaml")), "spec.removed", false))
	send(9, "unknown/method", map[string]any{})
	m = wait(func(m message) bool { return string(m.ID) == "9" })
	if m.Error == nil || m.Error.Code != -32601 {
		t.Fatal("unsupported method did not return protocol error")
	}
	send(3, "shutdown", nil)
	wait(func(m message) bool { return string(m.ID) == "3" })
	send(0, "exit", nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server failed to exit")
	}
}

func TestFramingRejectsInvalidLength(t *testing.T) {
	for _, input := range []string{"Content-Length: -1\r\n\r\n", "Content-Length: 999999999\r\n\r\n", "Other: 1\r\n\r\n"} {
		if _, err := read(bufio.NewReader(strings.NewReader(input))); err == nil {
			t.Fatal("invalid frame accepted")
		}
	}
}
