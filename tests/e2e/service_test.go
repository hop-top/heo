package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// daemon is one `heo service start` process.
type daemon struct {
	t      *testing.T
	cmd    *exec.Cmd
	mu     sync.Mutex
	stderr bytes.Buffer
	ready  map[string]string // service -> reported address
	done   chan struct{}
	exit   int
}

var readyRe = regexp.MustCompile(`ready_reported object=service .*service=([\w-]+)(?: address=(\S+))?`)

// start runs heo with args and waits until every service in want has
// reported ready. stdin is the given reader (nil: empty).
func (s *sandbox) start(env []string, stdin io.Reader, want []string, args ...string) *daemon {
	s.t.Helper()
	if runtime.GOOS == "windows" {
		s.t.Skip("the daemon is unix-only")
	}
	cmd := exec.Command(heoBin, args...) //nolint:gosec // runs the binary TestMain built
	cmd.Dir = s.path("cwd")
	cmd.Stdin = stdin
	for k, v := range s.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, env...)
	d := &daemon{t: s.t, cmd: cmd, ready: map[string]string{}, done: make(chan struct{})}
	pipe, err := cmd.StderrPipe()
	if err != nil {
		s.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	go func() {
		sc := bufio.NewScanner(pipe)
		for sc.Scan() {
			line := sc.Text()
			d.mu.Lock()
			d.stderr.WriteString(line + "\n")
			if m := readyRe.FindStringSubmatch(line); m != nil {
				d.ready[m[1]] = m[2]
			}
			d.mu.Unlock()
		}
		err := cmd.Wait()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			d.exit = ee.ExitCode()
		}
		close(d.done)
	}()
	s.t.Cleanup(func() {
		select {
		case <-d.done:
		default:
			_ = cmd.Process.Kill()
			<-d.done
		}
	})
	deadline := time.Now().Add(15 * time.Second)
	for {
		d.mu.Lock()
		missing := ""
		for _, w := range want {
			if _, ok := d.ready[w]; !ok {
				missing = w
				break
			}
		}
		d.mu.Unlock()
		if missing == "" {
			return d
		}
		select {
		case <-d.done:
			s.t.Fatalf("heo %v exited %d before %s was ready:\n%s", args, d.exit, missing, d.log())
		default:
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("heo %v: %s never ready:\n%s", args, missing, d.log())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (d *daemon) log() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stderr.String()
}

func (d *daemon) isReady(svc string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.ready[svc]
	return ok
}

func (d *daemon) addr(svc string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ready[svc]
}

// term sends SIGTERM and returns the exit code.
func (d *daemon) term() int {
	d.t.Helper()
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		d.t.Fatal(err)
	}
	return d.wait()
}

func (d *daemon) wait() int {
	d.t.Helper()
	select {
	case <-d.done:
	case <-time.After(15 * time.Second):
		d.t.Fatalf("daemon did not exit:\n%s", d.log())
	}
	return d.exit
}

// noRuntime disables the runtime services for a supervisor run that
// has no data store: they open one at start. runtime_test.go runs them.
const noRuntime = "--disable=workers,receivers,bridge"

// handedOut holds every address freeAddr returned in this process. A
// port picked by listening and closing is free again at once, so two
// picks in a row, such as a daemon's api and rpc addresses, can return
// the same port; the daemon then fails to bind the second. The test
// binary runs every test, so the set spans them all.
var (
	handedOutMu sync.Mutex
	handedOut   = map[string]bool{}
)

// freeAddr returns a loopback address nothing listens on and no earlier
// call returned.
func freeAddr(t *testing.T) string {
	t.Helper()
	return pickAddr(t, func() (string, error) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		defer func() { _ = l.Close() }()
		return l.Addr().String(), nil
	})
}

// pickAddr draws from next until it yields an address not handed out.
func pickAddr(t *testing.T, next func() (string, error)) string {
	t.Helper()
	handedOutMu.Lock()
	defer handedOutMu.Unlock()
	for range 1000 {
		addr, err := next()
		if err != nil {
			t.Fatal(err)
		}
		if !handedOut[addr] {
			handedOut[addr] = true
			return addr
		}
	}
	t.Fatal("no unused free address after 1000 draws")
	return ""
}

func TestServiceStartSocketOnlyAndSIGTERM(t *testing.T) {
	s := newSandbox(t)
	d := s.start(nil, nil, []string{"socket"}, "service", "start", noRuntime)
	if got, want := d.addr("socket"), s.path("run", "heo", "default", "heo.sock"); got != want {
		t.Errorf("socket at %s, want %s", got, want)
	}
	for _, svc := range []string{"api", "rpc", "mcp"} {
		if d.isReady(svc) {
			t.Errorf("%s started by default", svc)
		}
	}
	if code := d.term(); code != 0 {
		t.Fatalf("SIGTERM exit %d:\n%s", code, d.log())
	}
	if !strings.Contains(d.log(), "reason=clean-stop") {
		t.Errorf("no clean stop:\n%s", d.log())
	}
}

// TestServiceStartMCPAndRPCFlags: each flag kit's mcp and rpc services
// read reaches its service from `service start`.
func TestServiceStartMCPAndRPCFlags(t *testing.T) {
	t.Run("rpc-addr", func(t *testing.T) {
		s := newSandbox(t)
		auth := s.useHTTPAuth()
		addr := freeAddr(t)
		d := s.start(nil, nil, []string{"socket", "rpc"}, "-c", auth.config,
			"service", "start", noRuntime, "--enable", "rpc", "--rpc-addr", addr)
		if got := d.addr("rpc"); got != "http://"+addr {
			t.Errorf("rpc at %s, want %s", got, addr)
		}
		s.want(result{exit: d.term()}, 0)
	})
	t.Run("rpc-addr-beats-c-override", func(t *testing.T) {
		// A `service start` address flag is a per-run override the
		// service reads itself, top for its key like --socket and
		// --addr: it beats -c services.rpc.addr=.
		s := newSandbox(t)
		auth := s.useHTTPAuth()
		cAddr, flagAddr := freeAddr(t), freeAddr(t)
		d := s.start(nil, nil, []string{"rpc"}, "-c", auth.config, "-c", "services.rpc.addr="+cAddr,
			"service", "start", "rpc", "--rpc-addr", flagAddr)
		if got := d.addr("rpc"); got != "http://"+flagAddr {
			t.Errorf("rpc at %s, want the flag address %s", got, flagAddr)
		}
		s.want(result{exit: d.term()}, 0)
	})
	t.Run("mcp-addr", func(t *testing.T) {
		s := newSandbox(t)
		auth := s.useHTTPAuth()
		addr := freeAddr(t)
		d := s.start(nil, nil, []string{"mcp"}, "-c", auth.config, "service", "start", "mcp", "--mcp-addr", addr)
		if got := d.addr("mcp"); got != "http://"+addr+"/mcp" {
			t.Errorf("mcp at %s, want %s", got, addr)
		}
		s.want(result{exit: d.term()}, 0)
	})
	t.Run("stdio", func(t *testing.T) {
		s := newSandbox(t)
		// Over stdio, MCP relays to the daemon: one must be running.
		daemon := s.start(nil, nil, []string{"socket"}, "service", "start", noRuntime)
		defer func() { s.want(result{exit: daemon.term()}, 0) }()
		in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}` + "\n")
		ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, heoBin, "service", "start", "mcp", "--stdio") //nolint:gosec // runs the binary TestMain built
		cmd.Dir = s.path("cwd")
		for k, v := range s.env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stdin = in
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("stdio: %v\n%s", err, errb.String())
		}
		// Over stdio the reply is on stdout and the process ends with
		// its input; over HTTP it would listen and log an address.
		if !strings.Contains(out.String(), `"jsonrpc":"2.0","id":1,"result"`) {
			t.Errorf("no initialize reply on stdout: %q\n%s", out.String(), errb.String())
		}
		if strings.Contains(errb.String(), "address=http") {
			t.Errorf("stdio run bound an HTTP listener:\n%s", errb.String())
		}
	})
	t.Run("mcp-addr-with-stdio", func(t *testing.T) {
		s := newSandbox(t)
		args := []string{"service", "start", "mcp", "--stdio", "--mcp-addr", "127.0.0.1:1"}
		r := s.run(nil, args...)
		s.want(r, 2, args...)
		if !strings.Contains(r.stderr, "--mcp-addr applies to the http transport") {
			t.Errorf("stderr = %q", r.stderr)
		}
	})
}

// TestServiceStartAPIFlagsReachService: the api and socket services
// read their flags from the running command, so they need no repair.
func TestServiceStartAPIFlagsReachService(t *testing.T) {
	s := newSandbox(t)
	auth := s.useHTTPAuth()
	addr := freeAddr(t)
	sock := s.path("alt.sock")
	d := s.start(nil, nil, []string{"socket", "api"}, "-c", auth.config,
		"service", "start", noRuntime, "--enable", "api", "--addr", addr, "--socket", sock)
	if got := d.addr("api"); got != addr {
		t.Errorf("api at %s, want %s", got, addr)
	}
	if got := d.addr("socket"); got != sock {
		t.Errorf("socket at %s, want %s", got, sock)
	}
	s.want(result{exit: d.term()}, 0)
}

// TestNoAuthFlagRefused: heo refuses kit's unauthenticated-serving
// flags on `service start` and on its deprecated `serve` spelling.
func TestNoAuthFlagRefused(t *testing.T) {
	s := newSandbox(t)
	for _, run := range [][]string{{"service", "start"}, {"serve"}} {
		for _, flag := range []string{"--no-auth", "--insecure-remote", "--insecure-no-policy"} {
			args := append(append([]string{}, run...), flag, "--format", "json")
			r := s.run(nil, args...)
			s.want(r, 2, args...)
			// Kit mounts --no-auth because the api service has an
			// authenticator (the surface token); heo refuses it.
			if !strings.Contains(r.stderr, `"code": "USAGE"`) || !strings.Contains(r.stderr, flag+" is refused") {
				t.Errorf("%v: not refused by heo: %q", args, r.stderr)
			}
		}
	}
	if _, err := os.Stat(s.path("run", "heo")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused start created the runtime dir: %v", err)
	}
}

// TestServiceStartNotInvocableOnServedSurfaces: no served surface runs
// `service start` or its deprecated `serve` spelling. The rest of the
// service group is kit's management vocabulary, withheld from every
// served surface, the socket included; heo's own `daemon ping` runs
// over the profile's heo socket (it is the CLI's path to the daemon)
// and is withheld from REST, gRPC and MCP.
func TestServiceStartNotInvocableOnServedSurfaces(t *testing.T) {
	s := newSandbox(t)
	auth := s.useHTTPAuth()
	token := auth.token(me())
	api, rpc, mcp := freeAddr(t), freeAddr(t), freeAddr(t)
	d := s.start(nil, nil, []string{"socket", "api", "rpc", "mcp"}, "-c", auth.config, "service", "start", noRuntime,
		"--enable", "api", "--enable", "rpc", "--enable", "mcp",
		"--addr", api, "--rpc-addr", rpc, "--mcp-addr", mcp)
	defer func() { s.want(result{exit: d.term()}, 0) }()

	t.Run("socket", func(t *testing.T) {
		for _, path := range [][]string{{"service", "start"}, {"serve"}, {"service", "setup"}, {"service", "ping"}} {
			if resp := socketCall(t, d.addr("socket"), path, nil); resp["ok"] != false {
				t.Errorf("socket ran %v: %v", path, resp)
			}
		}
		if resp := socketCall(t, d.addr("socket"), []string{"daemon", "ping"}, nil); resp["ok"] != true {
			t.Errorf("socket refused daemon ping: %v", resp)
		}
	})

	t.Run("rest", func(t *testing.T) {
		resp, err := authed(http.MethodGet, "http://"+api+"/v1/commands", token, "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var doc struct {
			Commands []struct {
				Name      string  `json:"name"`
				Invocable bool    `json:"invocable"`
				Reason    *string `json:"reason"`
			} `json:"commands"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, c := range doc.Commands {
			switch c.Name {
			case "service start":
				seen[c.Name] = true
				if c.Invocable || c.Reason == nil || *c.Reason != "self-hosting" {
					t.Errorf("discovery: service start invocable=%v reason=%v", c.Invocable, c.Reason)
				}
			case "serve", "service status", "service stop", "service restart", "service list", "service setup", "daemon ping":
				if c.Invocable {
					t.Errorf("discovery: %s invocable", c.Name)
				}
			}
		}
		if !seen["service start"] {
			t.Errorf("discovery misses service start: %v", seen)
		}
		post, err := authed(http.MethodPost, "http://"+api+"/v1/commands/service/start", token, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		_ = post.Body.Close()
		if post.StatusCode < 400 {
			t.Errorf("REST ran service start: %d", post.StatusCode)
		}
	})

	t.Run("rpc", func(t *testing.T) {
		invoke := func(path ...string) (int, string) {
			body, _ := json.Marshal(map[string]any{"path": path})
			resp, err := authed(http.MethodPost, "http://"+rpc+"/cmdsurface.v1.Commands/Invoke", token, string(body))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b)
		}
		if code, body := invoke("service", "start"); code < 400 {
			t.Errorf("rpc ran service start: %d %s", code, body)
		}
		if code, body := invoke("service", "stop"); code < 400 {
			t.Errorf("rpc ran service stop: %d %s", code, body)
		}
		if code, body := invoke("service", "setup", "api"); code < 400 {
			t.Errorf("rpc ran service setup: %d %s", code, body)
		}
		if code, body := invoke("daemon", "ping"); code < 400 {
			t.Errorf("rpc ran daemon ping: %d %s", code, body)
		}
	})

	t.Run("mcp", func(t *testing.T) {
		tools := mcpToolNames(t, "http://"+mcp+"/mcp", token)
		for name := range tools {
			if strings.HasPrefix(name, "service.") || name == "serve" || name == "daemon.ping" {
				t.Errorf("MCP lists %s: %v", name, tools)
			}
		}
		if !tools["config.get"] {
			t.Errorf("MCP lacks config.get: %v", tools)
		}
	})
}
