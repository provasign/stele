package cli

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
)

// serving is set in `stele serve`: registries stay loaded and lowered until
// one of their files changes.
var serving bool

type loaded struct {
	print string
	reg   *ir.Registry
	prog  *lower.Program
}

var (
	loadedMu sync.Mutex
	programs = map[string]loaded{} // by absolute dir and lowering options
)

// loadProgram loads and lowers the registry in dir. In `stele serve` it
// answers from memory while the registry's files are unchanged.
func loadProgram(dir string, opt lower.Options) (*ir.Registry, *lower.Program, error) {
	if !serving {
		reg, err := ir.LoadDir(dir)
		if err != nil {
			return nil, nil, err
		}
		prog, err := lower.Lower(reg, opt)
		return reg, prog, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, err
	}
	// The path as given is in the key too: locations print the way it names
	// the files, as a one-shot check prints them.
	key := fmt.Sprintf("%s %s %+v", abs, dir, opt)
	print, err := fingerprint(abs)
	if err != nil {
		return nil, nil, err
	}
	loadedMu.Lock()
	defer loadedMu.Unlock()
	if l, ok := programs[key]; ok && l.print == print {
		return l.reg, l.prog, nil
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	prog, err := lower.Lower(reg, opt)
	if err != nil {
		return nil, nil, err
	}
	programs[key] = loaded{print, reg, prog}
	return reg, prog, nil
}

// fingerprint names the YAML files ir.LoadDir reads under dir, with their
// sizes and modification times.
func fingerprint(dir string) (string, error) {
	var b bytes.Buffer
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ext := filepath.Ext(path); !d.IsDir() && (ext == ".yaml" || ext == ".yml") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s %d %d\n", p, fi.Size(), fi.ModTime().UnixNano())
	}
	return b.String(), nil
}

// socketPath is where `stele serve` listens: STELE_SOCKET, or beside the
// persisted cache.
func socketPath() string {
	if p := os.Getenv("STELE_SOCKET"); p != "" {
		return p
	}
	if checker.PersistDir == "" {
		return ""
	}
	p := filepath.Join(filepath.Dir(checker.PersistDir), "serve.sock")
	if len(p) > 100 {
		// Unix socket paths are limited to about 104 bytes.
		sum := sha256.Sum256([]byte(p))
		p = filepath.Join(os.TempDir(), fmt.Sprintf("stele-%d-%x.sock", os.Getuid(), sum[:6]))
	}
	return p
}

type request struct {
	Build string   `json:"build"`
	Dir   string   `json:"dir"` // the client's working directory
	Args  []string `json:"args"`
}

type response struct {
	Refused string `json:"refused,omitempty"` // not answered; the client runs the command itself
	Stdout  string `json:"stdout"`
	Stderr  string `json:"stderr"`
	Code    int    `json:"code"`
}

// cmdServe answers `stele check` for clients on a unix socket, one at a
// time, keeping registries, warm solvers and results in memory.
func cmdServe(args []string, log io.Writer) int {
	path := socketPath()
	if len(args) > 0 {
		path = args[0]
	}
	if path == "" {
		fmt.Fprintln(log, "stele serve: no socket path (set STELE_SOCKET or a cache directory)")
		return 2
	}
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		fmt.Fprintf(log, "stele serve: already serving on %s\n", path)
		return 2
	}
	os.Remove(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(log, "stele serve: %v\n", err)
		return 2
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		fmt.Fprintf(log, "stele serve: %v\n", err)
		return 2
	}
	defer ln.Close()
	serving = true
	checker.SharedCache = checker.DefaultCache()
	fmt.Fprintf(log, "stele serve: listening on %s\n", path)
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(log, "stele serve: %v\n", err)
			return 2
		}
		answer(conn, log)
	}
}

func answer(conn net.Conn, log io.Writer) {
	start := time.Now()
	defer conn.Close()
	var req request
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
		return
	}
	var resp response
	switch {
	case req.Build != checker.BuildID():
		resp.Refused = "a different stele build"
	case len(req.Args) == 0 || req.Args[0] != "check":
		resp.Refused = "not a served command"
	default:
		// One request at a time: relative paths resolve from the client's
		// working directory.
		if err := os.Chdir(req.Dir); err != nil {
			resp.Refused = err.Error()
			break
		}
		var out, errOut bytes.Buffer
		resp.Code = checkCmd(req.Args[1:], &out, &errOut)
		resp.Stdout, resp.Stderr = out.String(), errOut.String()
	}
	json.NewEncoder(conn).Encode(resp)
	if os.Getenv("STELE_SERVE_LOG") != "" {
		fmt.Fprintf(log, "stele serve: %v in %v (exit %d)\n", req.Args, time.Since(start).Round(10*time.Microsecond), resp.Code)
	}
}

// viaServer runs args in a `stele serve` of this build when one is
// listening; ok is false when the caller must run them itself.
func viaServer(args []string) (code int, ok bool) {
	path := socketPath()
	if path == "" || os.Getenv("STELE_SERVE") == "off" {
		return 0, false
	}
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		return 0, false
	}
	defer conn.Close()
	wd, err := os.Getwd()
	if err != nil {
		return 0, false
	}
	if err := json.NewEncoder(conn).Encode(request{Build: checker.BuildID(), Dir: wd, Args: args}); err != nil {
		return 0, false
	}
	var resp response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil || resp.Refused != "" {
		return 0, false
	}
	io.WriteString(os.Stdout, resp.Stdout)
	io.WriteString(os.Stderr, resp.Stderr)
	return resp.Code, true
}
