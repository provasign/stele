// Package lsp exposes saved-spec synchronization through the Language Server
// Protocol. Stdout carries protocol frames only; watching invokes no models.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/provasign/stele/internal/specsync"
)

type Server struct {
	Resolve  func(workspace string) (specsync.Options, error)
	Interval time.Duration
}

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
type writer struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *writer) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err = fmt.Fprintf(w.out, "Content-Length: %d\r\n\r\n", len(b)); err != nil {
		return err
	}
	_, err = w.out.Write(b)
	return err
}
func read(r *bufio.Reader) (message, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return message{}, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(k, "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return message{}, err
			}
		}
	}
	if length < 0 || length > 16<<20 {
		return message{}, fmt.Errorf("invalid LSP Content-Length")
	}
	b := make([]byte, length)
	if _, err := io.ReadFull(r, b); err != nil {
		return message{}, err
	}
	var m message
	err := json.Unmarshal(b, &m)
	return m, err
}

func FileURI(path string) string {
	path, _ = filepath.Abs(path)
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}
func filePath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	if u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") {
		return "", fmt.Errorf("workspace must use a local file URI")
	}
	p := filepath.FromSlash(u.Path)
	if len(p) > 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return p, nil
}

func (s Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	interval := s.Interval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	w := &writer{out: out}
	r := bufio.NewReader(in)
	var options *specsync.Options
	var watchCancel context.CancelFunc
	shutdown, started := false, false
	respond := func(id json.RawMessage, result any, err error) error {
		m := map[string]any{"jsonrpc": "2.0", "id": id}
		if err != nil {
			m["error"] = map[string]any{"code": -32602, "message": err.Error()}
		} else {
			m["result"] = result
		}
		return w.send(m)
	}
	for {
		m, err := read(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch m.Method {
		case "initialize":
			if options != nil {
				if err := respond(m.ID, nil, fmt.Errorf("already initialized")); err != nil {
					return err
				}
				continue
			}
			var p struct {
				RootURI  string `json:"rootUri"`
				RootPath string `json:"rootPath"`
			}
			if err = json.Unmarshal(m.Params, &p); err != nil {
				if err = respond(m.ID, nil, err); err != nil {
					return err
				}
				continue
			}
			workspace := p.RootPath
			if p.RootURI != "" {
				workspace, err = filePath(p.RootURI)
			}
			var opt specsync.Options
			if err == nil {
				opt, err = s.Resolve(workspace)
			}
			if err == nil {
				err = opt.Validate()
			}
			if err != nil {
				if err = respond(m.ID, nil, err); err != nil {
					return err
				}
				continue
			}
			options = &opt
			caps := map[string]any{"capabilities": map[string]any{"textDocumentSync": map[string]any{"openClose": false, "change": 0, "save": true}, "executeCommandProvider": map[string]any{"commands": []string{"stele.sync"}}}, "serverInfo": map[string]string{"name": "Stele"}}
			if err = respond(m.ID, caps, nil); err != nil {
				return err
			}
		case "initialized":
			if options == nil || started || shutdown {
				continue
			}
			started = true
			var watchCtx context.Context
			watchCtx, watchCancel = context.WithCancel(ctx)
			defer watchCancel()
			go func(opt specsync.Options) {
				published := map[string]bool{}
				_ = specsync.Watch(watchCtx, opt, interval, func(result *specsync.Result, err error) {
					if watchCtx.Err() != nil {
						return
					}
					if err != nil {
						_ = w.send(map[string]any{"jsonrpc": "2.0", "method": "window/showMessage", "params": map[string]any{"type": 1, "message": "Stele sync: " + err.Error()}})
						return
					}
					groups := map[string][]map[string]any{}
					for _, d := range result.Diagnostics {
						uri := FileURI(d.File)
						line := max(0, d.Line-1)
						if data, e := os.ReadFile(d.File); e == nil {
							line = min(line, strings.Count(string(data), "\n"))
						}
						severity := 2
						if d.Code == "rule.stale" || d.Code == "rule.orphaned" || d.Code == "spec.ambiguous" || d.Code == "spec.root_missing" {
							severity = 1
						}
						if d.Code == "spec.moved" || d.Code == "spec.provenance" {
							severity = 3
						}
						position := map[string]int{"line": line, "character": 0}
						groups[uri] = append(groups[uri], map[string]any{"range": map[string]any{"start": position, "end": position}, "severity": severity, "code": d.Code, "source": "Stele", "message": d.Message, "data": map[string]any{"statement": d.Statement, "rules": d.Rules}})
					}
					for uri := range published {
						if groups[uri] == nil {
							groups[uri] = []map[string]any{}
						}
					}
					uris := make([]string, 0, len(groups))
					for uri := range groups {
						uris = append(uris, uri)
					}
					sort.Strings(uris)
					for _, uri := range uris {
						if err := w.send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": map[string]any{"uri": uri, "diagnostics": groups[uri]}}); err != nil {
							cancel()
							return
						}
						if len(groups[uri]) > 0 {
							published[uri] = true
						} else {
							delete(published, uri)
						}
					}
				})
			}(*options)
		case "workspace/executeCommand":
			var p struct {
				Command string `json:"command"`
			}
			err = json.Unmarshal(m.Params, &p)
			var result *specsync.Result
			if err == nil && (options == nil || shutdown || p.Command != "stele.sync") {
				err = fmt.Errorf("initialize Stele and use command stele.sync")
			}
			if err == nil {
				result, err = specsync.Evaluate(*options)
			}
			if err = respond(m.ID, result, err); err != nil {
				return err
			}
		case "shutdown":
			shutdown = true
			if watchCancel != nil {
				watchCancel()
			}
			if err = respond(m.ID, nil, nil); err != nil {
				return err
			}
		case "exit":
			if !shutdown {
				return fmt.Errorf("LSP exit received before shutdown")
			}
			return nil
		default:
			// Saves and workspace file notifications need no response. The
			// content watcher also catches writes by coding CLIs and external tools.
			if len(m.ID) > 0 {
				if err = w.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "method not supported"}}); err != nil {
					return err
				}
			}
		}
	}
}
