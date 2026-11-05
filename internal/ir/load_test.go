package ir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A proposal file may extend an entity, or declare an operation on it,
// before the file that declares the entity is read.
func TestExtendAndOperationsResolveAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a-proposed.yaml", `extend: Workspace
context: c
attributes:
  dnsRecordExists: { type: bool }
relations:
  node: { to: Node, inverse: workspaces }
---
operation: Workspace.stop
context: c
default: allow
writes: [Workspace.status, Workspace.dnsRecordExists]
`)
	write("b-domain.yaml", `context: c
formatVersion: 1
---
entity: Workspace
context: c
attributes:
  status: { type: enum, values: [running, stopped] }
---
entity: Node
context: c
attributes:
  vmExists: { type: bool }
`)
	reg, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ws := reg.Entities["Workspace"]
	if ws.Attrs["dnsRecordExists"] == nil || ws.Relations["node"] == nil || reg.Entities["Node"].Relations["workspaces"] == nil {
		t.Errorf("extension not applied: %+v", ws)
	}
	op := reg.Operations["Workspace.stop"]
	if op == nil || len(op.Writes) != 2 || ws.Attrs["dnsRecordExists"].Controller != System {
		t.Errorf("operation not resolved: %+v", op)
	}

	write("c-bad.yaml", "extend: Nowhere\ncontext: c\n---\noperation: Nowhere.go\ncontext: c\ndefault: allow\nwrites: []\n")
	_, err = LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), `extend "Nowhere": no such entity`) || !strings.Contains(err.Error(), "operation Nowhere.go: no entity Nowhere") {
		t.Errorf("err = %v", err)
	}
}

// An operation extension's keeps take back a write another file declared,
// whichever file is read first; a value the step's effect sets cannot be
// kept.
func TestExtensionKeepsAWrite(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a-kept.yaml", "extend: Tool.list\nkeeps: [Tool.name]\n")
	write("b-domain.yaml", `context: c
formatVersion: 1
---
entity: Tool
context: c
attributes:
  name: { type: string }
  shown: { type: bool }
---
operation: Tool.list
context: c
default: allow
writes: [Tool.name, Tool.shown]
effect:
  shown: "true"
`)
	reg, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	op := reg.Operations["Tool.list"]
	if len(op.Writes) != 1 || op.Writes[0].Name != "shown" {
		t.Errorf("writes = %v, want Tool.shown alone", op.Writes)
	}
	write("c-effect.yaml", "extend: Tool.list\nkeeps: [Tool.shown]\n")
	if _, err = LoadDir(dir); err == nil || !strings.Contains(err.Error(), "keeps Tool.shown, which its effect sets") {
		t.Errorf("err = %v", err)
	}
}

// Every type spelling a translator may write loads to the same types.
func TestTypeSpellings(t *testing.T) {
	dir := t.TempDir()
	src := `context: c
formatVersion: 1
---
entity: Job
context: c
attributes:
  route: { type: string }
  ratio: { type: decimal, range: [0, 0.5] }
  idle: { type: duration }
  at: { type: timestamp }
  owner: { type: optional<string> }
  tags: { type: "list<enum[Hot, cold]>", maxLength: 2 }
  perms: { type: set, of: enum, values: [read, write] }
  limits: { type: "map<enum[gold, silver], int>", range: [0, 3] }
  count: { type: int }
  result.code: { type: int, range: [0, 2] }
  result.ok: { type: bool }
relations:
  workers: { to: Worker, many: true }
---
entity: Worker
context: c
attributes:
  busy: { type: bool }
---
operation: Job.finish
context: c
default: allow
writes: [Job.result.code, Job.tags]
`
	if err := os.WriteFile(filepath.Join(dir, "r.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	job := reg.Entities["Job"]
	got := map[string]string{}
	for _, n := range job.AttrNames() {
		got[n] = job.Attrs[n].TypeString()
	}
	want := map[string]string{
		"route": "string", "ratio": "decimal 0..0.5", "idle": "duration", "at": "time", "owner": "optional string",
		"tags": "list<enum {Hot, cold}> (up to 2)", "perms": "set<enum {read, write}>",
		"limits": "map<enum {gold, silver}, int 0..3>", "count": "int",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	code := reg.Entities["Job$result"].Attrs["code"]
	if code == nil || code.Path() != "Job.result.code" || code.Owner() != "Job" || !reg.Operations["Job.finish"].WritesAttr(code) {
		t.Errorf("record field: %+v", code)
	}
	if w := job.Relations["workers"]; !w.Many || w.Max != DefaultMaxInstances {
		t.Errorf("relation to many: %+v", w)
	}
}
