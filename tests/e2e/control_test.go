package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// sockCall sends one request over the control socket at path and
// returns the decoded response line.
func sockCall(t *testing.T, path string, req map[string]any) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err, string(line))
	}
	return resp
}

// resultData returns a socket response's result.data, failing unless
// the command ran and exited 0.
func resultData(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	res, _ := resp["result"].(map[string]any)
	if resp["ok"] != true || res == nil || res["exit_code"] != float64(0) {
		t.Fatalf("socket call did not succeed: %v", resp)
	}
	data, _ := res["data"].(map[string]any)
	if data == nil {
		t.Fatalf("no data: %v", resp)
	}
	return data
}

// ping is `daemon ping --format json` decoded.
type ping struct {
	Profile string `json:"profile"`
	Tenant  string `json:"tenant"`
	Actor   string `json:"actor"`
	Surface string `json:"surface"`
	PID     int    `json:"pid"`
	Runtime struct {
		Ready   bool `json:"ready"`
		Storage struct {
			State string `json:"state"`
		} `json:"storage"`
		Services []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"services"`
	} `json:"runtime"`
}

func decodePing(t *testing.T, raw string) ping {
	t.Helper()
	var p ping
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("ping output: %v\n%s", err, raw)
	}
	return p
}

// TestDaemonAbsentExits70: with no daemon running a daemon command
// fails with PREREQUISITE, exit 70, naming the command that starts
// one; nothing is started and no runtime directory is created.
func TestDaemonAbsentExits70(t *testing.T) {
	s := newSandbox(t)
	args := []string{"daemon", "ping", "--format", "json"}
	r := s.run(nil, args...)
	s.want(r, 70, args...)
	var env struct {
		Code         string `json:"code"`
		SuggestedFix string `json:"suggested_fix"`
		ExitCode     int    `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(r.stderr), &env); err != nil {
		t.Fatalf("stderr is not an envelope: %v\n%s", err, r.stderr)
	}
	if env.Code != "PREREQUISITE" || env.ExitCode != 70 ||
		!strings.Contains(env.SuggestedFix, "heo service start") {
		t.Errorf("envelope = %s", r.stderr)
	}
	if _, err := os.Stat(s.path("run", "heo")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a client run created the runtime dir: %v", err)
	}

	// A named profile's fix names that profile.
	s.write("cfg/heo/profiles/work.yaml", "")
	r = s.run(nil, "--profile", "work", "daemon", "ping")
	s.want(r, 70, "--profile", "work", "daemon", "ping")
	if !strings.Contains(r.stderr, "PREREQUISITE") || !strings.Contains(r.stderr, "--profile heo://profile/work service start") {
		t.Errorf("stderr = %s", r.stderr)
	}
}

// TestOfflineCommandsNeedNoDaemon: help, config, spec and the service
// listing run with no daemon and never reach for one.
func TestOfflineCommandsNeedNoDaemon(t *testing.T) {
	s := newSandbox(t)
	for _, args := range [][]string{
		{"--help"},
		{"daemon", "ping", "--help"},
		{"config", "list"},
		{"config", "validate"},
		{"spec"},
		{"service", "list"},
	} {
		s.want(s.run(nil, args...), 0, args...)
	}
	if _, err := os.Stat(s.path("run", "heo")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an offline command created the runtime dir: %v", err)
	}
}

// TestForeignOwnedSocketRefused: the client refuses, with UNAUTHORIZED
// before sending anything, a control socket in a directory another
// user owns, or one other users can enter.
func TestForeignOwnedSocketRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the control socket is unix-only")
	}
	t.Run("directory owned by another user", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root owns /")
		}
		s := newSandbox(t)
		args := []string{"-c", "services.socket.path=/heo.sock", "daemon", "ping"}
		r := s.run(nil, args...)
		s.want(r, 5, args...)
		if !strings.Contains(r.stderr, "UNAUTHORIZED") || !strings.Contains(r.stderr, "owned by uid 0") {
			t.Errorf("stderr = %s", r.stderr)
		}
	})
	t.Run("directory open to other users", func(t *testing.T) {
		s := newSandbox(t)
		dir := s.path("shared")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "heo.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		accepted := make(chan struct{}, 1)
		go func() {
			if c, err := ln.Accept(); err == nil {
				accepted <- struct{}{}
				_ = c.Close()
			}
		}()
		if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the test's squatted directory
			t.Fatal(err)
		}
		args := []string{"-c", "services.socket.path=" + path, "daemon", "ping"}
		r := s.run(nil, args...)
		s.want(r, 5, args...)
		if !strings.Contains(r.stderr, "open to other users") || !strings.Contains(r.stderr, "chmod 700") {
			t.Errorf("stderr = %s", r.stderr)
		}
		select {
		case <-accepted:
			t.Error("the client connected to the squatted socket")
		case <-time.After(100 * time.Millisecond):
		}
	})
}

// TestControlSocketDerivesActorServerSide: the daemon names the caller
// by its socket peer uid and acts for the profile's tenant; a caller
// and tenant the request claims are ignored. The CLI gets the same
// answer through its client.
func TestControlSocketDerivesActorServerSide(t *testing.T) {
	s := newSandbox(t)
	d := s.start(nil, nil, []string{"socket"}, "-c", "runtime.tenant=acme", "service", "start", noRuntime)
	defer func() { s.want(result{exit: d.term()}, 0) }()

	data := resultData(t, sockCall(t, d.addr("socket"), map[string]any{
		"path": []string{"daemon", "ping"}, "caller": "uid:0", "tenant": "evil",
	}))
	if data["actor"] != me() || data["tenant"] != "acme" || data["surface"] != "socket" {
		t.Errorf("socket ping = %v, want actor %s tenant acme", data, me())
	}

	r := s.run(nil, "daemon", "ping", "--format", "json")
	s.want(r, 0, "daemon", "ping")
	p := decodePing(t, r.stdout)
	if p.Actor != me() || p.Tenant != "acme" || p.Surface != "socket" || p.PID != d.cmd.Process.Pid {
		t.Errorf("CLI ping = %+v, want the daemon's (pid %d) view of %s", p, d.cmd.Process.Pid, me())
	}
	if p.Runtime.Storage.State != "closed" {
		t.Errorf("storage = %q with no runtime service, want closed", p.Runtime.Storage.State)
	}

	text := s.run(nil, "daemon", "ping")
	s.want(text, 0, "daemon", "ping")
	for _, want := range []string{"actor", me(), "tenant", "acme", "workers"} {
		if !strings.Contains(text.stdout, want) {
			t.Errorf("text output lacks %q:\n%s", want, text.stdout)
		}
	}

	// An unknown flag is the daemon's usage error, rendered here.
	u := s.run(nil, "daemon", "ping", "--bogus")
	s.want(u, 2, "daemon", "ping", "--bogus")
}

// TestPingReportsRuntimeState: against a running runtime, ping reports
// the store and runtime services as the daemon holds them.
func TestPingReportsRuntimeState(t *testing.T) {
	db := newDB(t).migrate()
	s := newSandbox(t)
	s.useStore(db)
	d := s.start(nil, nil, []string{"socket", "workers", "receivers"}, "service", "start")
	defer func() { s.want(result{exit: d.term()}, 0) }()
	r := s.run(nil, "daemon", "ping", "--format", "json")
	s.want(r, 0, "daemon", "ping")
	p := decodePing(t, r.stdout)
	if !p.Runtime.Ready || p.Runtime.Storage.State != "ready" {
		t.Errorf("runtime = %+v, want ready on a ready store", p.Runtime)
	}
	states := map[string]string{}
	for _, svc := range p.Runtime.Services {
		states[svc.Name] = svc.State
	}
	if states["workers"] != "ready" || states["receivers"] != "ready" || states["webhook"] != "not_started" {
		t.Errorf("services = %v", states)
	}
}

// TestBareServeOpensNoTCPListener: a bare start serves the control
// socket and the runtime, and no TCP surface.
func TestBareServeOpensNoTCPListener(t *testing.T) {
	s := newSandbox(t)
	d := s.start(nil, nil, []string{"socket"}, "service", "start", noRuntime)
	defer func() { s.want(result{exit: d.term()}, 0) }()
	for _, svc := range []string{"api", "rpc", "mcp"} {
		if d.isReady(svc) {
			t.Errorf("bare start served %s", svc)
		}
	}
	if strings.Contains(d.log(), "address=http") || strings.Contains(d.log(), "address=127.") {
		t.Errorf("bare start logged a TCP address:\n%s", d.log())
	}
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return
	}
	out, _ := exec.Command(lsof, "-a", "-p", strconv.Itoa(d.cmd.Process.Pid), "-iTCP", "-sTCP:LISTEN", "-Fn").Output() //nolint:gosec // test pid
	if strings.Contains(string(out), "n") && strings.Contains(string(out), ":") {
		t.Errorf("daemon listens on TCP:\n%s", out)
	}
}

// TestHTTPSurfacesRequireAuthOnLoopback: REST, gRPC and MCP over HTTP
// refuse to start without an authenticator, loopback included; with
// one, an unauthenticated request is turned away at the edge.
func TestHTTPSurfacesRequireAuthOnLoopback(t *testing.T) {
	s := newSandbox(t)
	for _, args := range [][]string{
		{"service", "start", "api", "--addr", "127.0.0.1:1"},
		{"service", "start", "rpc", "--rpc-addr", "127.0.0.1:1"},
		{"service", "start", "mcp", "--mcp-addr", "127.0.0.1:1"},
		{"service", "start", noRuntime, "--enable", "api"},
		{"-c", "services.rpc.enabled=true", "service", "start", noRuntime},
	} {
		r := s.run(nil, args...)
		s.want(r, 2, args...)
		if !strings.Contains(r.stderr, "no authenticator configured") || readyRe.MatchString(r.stderr) {
			t.Errorf("%v: stderr = %s", args, r.stderr)
		}
	}
	auth := s.useHTTPAuth()
	api := freeAddr(t)
	d := s.start(nil, nil, []string{"socket", "api"}, "-c", auth.config,
		"service", "start", noRuntime, "--enable", "api", "--addr", api)
	defer func() { s.want(result{exit: d.term()}, 0) }()
	resp, err := authed(http.MethodGet, "http://"+api+"/v1/commands", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated discovery = %d, want 401", resp.StatusCode)
	}
	resp, err = authed(http.MethodGet, "http://"+api+"/v1/commands", auth.token(me()), "")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("authenticated discovery = %d, want 200", resp.StatusCode)
	}
}

// surfaces is a daemon serving the socket and every HTTP surface,
// with JWT authentication.
type surfaces struct {
	d             *daemon
	auth          *httpAuth
	api, rpc, mcp string
}

func startSurfaces(t *testing.T, s *sandbox) *surfaces {
	t.Helper()
	auth := s.useHTTPAuth()
	api, rpc, mcp := freeAddr(t), freeAddr(t), freeAddr(t)
	d := s.start(nil, nil, []string{"socket", "api", "rpc", "mcp"}, "-c", auth.config,
		"service", "start", noRuntime, "--enable", "api", "--enable", "rpc", "--enable", "mcp",
		"--addr", api, "--rpc-addr", rpc, "--mcp-addr", mcp)
	return &surfaces{d: d, auth: auth, api: api, rpc: rpc, mcp: mcp}
}

// rest calls a command over REST as the bearer of token.
func (x *surfaces) rest(t *testing.T, method, route, token, body string) (int, map[string]any) {
	t.Helper()
	resp, err := authed(method, "http://"+x.api+route, token, body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

// invokeRPC calls cmdsurface.v1.Commands/Invoke over Connect JSON.
func (x *surfaces) invokeRPC(t *testing.T, token string, req map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(req)
	resp, err := authed(http.MethodPost, "http://"+x.rpc+"/cmdsurface.v1.Commands/Invoke", token, string(b))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return resp.StatusCode, out
}

// TestServedPermissionGateParity: a daemon command answers with the
// same schema, the same actor and the same tenant over the socket,
// REST, gRPC and MCP; each surface names itself. `daemon ping` is
// served over the socket only, so the e2e build's `e2e whoami` stands
// in for it.
func TestServedPermissionGateParity(t *testing.T) {
	withE2EBinary(t, func() { servedPermissionGateParity(t) })
}

func servedPermissionGateParity(t *testing.T) {
	s := newSandbox(t)
	x := startSurfaces(t, s)
	defer func() { s.want(result{exit: x.d.term()}, 0) }()
	token := x.auth.token(me())
	path := []string{"e2e", "whoami"}

	views := map[string]map[string]any{}
	views["socket"] = resultData(t, sockCall(t, x.d.addr("socket"), map[string]any{"path": path}))

	code, body := x.rest(t, http.MethodGet, "/v1/commands/e2e/whoami", token, "")
	if code != http.StatusOK {
		t.Fatalf("REST whoami = %d %v", code, body)
	}
	views["rest"], _ = body["data"].(map[string]any)

	code, body = x.invokeRPC(t, token, map[string]any{"path": path})
	if code != http.StatusOK {
		t.Fatalf("rpc whoami = %d %v", code, body)
	}
	var rpcData map[string]any
	if raw, _ := body["dataJson"].(string); raw != "" {
		_ = json.Unmarshal([]byte(raw), &rpcData)
	} else if raw, _ := body["data_json"].(string); raw != "" {
		_ = json.Unmarshal([]byte(raw), &rpcData)
	}
	views["rpc"] = rpcData

	m := newMCPClient(t, "http://"+x.mcp+"/mcp", token)
	views["mcp"] = m.callTool(t, "e2e.whoami", map[string]any{})

	wantSurface := map[string]string{"socket": "socket", "rest": "rest", "rpc": "rpc", "mcp": "mcp"}
	var keys []string
	for name, v := range views {
		if v == nil {
			t.Errorf("%s: no data", name)
			continue
		}
		if v["actor"] != me() || v["tenant"] != "default" || v["surface"] != wantSurface[name] ||
			v["pid"] != float64(x.d.cmd.Process.Pid) {
			t.Errorf("%s: %v, want actor %s, tenant default, surface %s", name, v, me(), wantSurface[name])
		}
		k := sortedKeys(v)
		if keys == nil {
			keys = k
		} else if !reflect.DeepEqual(k, keys) {
			t.Errorf("%s: keys %v, want %v", name, k, keys)
		}
	}

	// One schema: every surface's data carries exactly the properties
	// of the output schema the command declares.
	if schema := specOutputProps(t, s, path...); !reflect.DeepEqual(schema, keys) {
		t.Errorf("data keys %v, declared schema %v", keys, schema)
	}

	// The socket's ping reports the same caller.
	ping := resultData(t, sockCall(t, x.d.addr("socket"), map[string]any{"path": []string{"daemon", "ping"}}))
	if ping["actor"] != me() || ping["tenant"] != "default" || ping["surface"] != "socket" {
		t.Errorf("socket ping = %v", ping)
	}

	// A token naming another user is that actor, not the socket's: one
	// the policy grants nothing is refused alike on every HTTP surface,
	// with the policy's reason, before the command runs.
	stranger := x.auth.token("uid:4242")
	code, body = x.rest(t, http.MethodGet, "/v1/commands/e2e/whoami", stranger, "")
	if code != http.StatusForbidden || !strings.Contains(asString(body), "heo.authz.no_role") {
		t.Errorf("REST whoami as uid:4242 = %d %v", code, body)
	}
	code, body = x.invokeRPC(t, stranger, map[string]any{"path": path})
	if code < 400 || !strings.Contains(strings.ToLower(asString(body)), "permission_denied") ||
		!strings.Contains(asString(body), "heo.authz.no_role") {
		t.Errorf("rpc whoami as uid:4242 = %d %v", code, body)
	}
	res := newMCPClient(t, "http://"+x.mcp+"/mcp", stranger).request(t, "tools/call",
		map[string]any{"name": "e2e.whoami", "arguments": map[string]any{}})
	if res["isError"] != true || !strings.Contains(asString(res), "heo.authz.no_role") {
		t.Errorf("MCP whoami as uid:4242 = %v", res)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// specOutputProps returns the property names of the output schema
// the spec manifest declares for the command at path, sorted.
func specOutputProps(t *testing.T, s *sandbox, path ...string) []string {
	t.Helper()
	r := s.run(nil, "spec", "--format", "json")
	s.want(r, 0, "spec")
	var doc struct {
		Commands []struct {
			Path   []string       `json:"path"`
			Output map[string]any `json:"output_schema"`
		} `json:"commands"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatal(err)
	}
	want := strings.Join(append([]string{"heo"}, path...), " ")
	for _, c := range doc.Commands {
		if strings.Join(c.Path, " ") != want {
			continue
		}
		schema := c.Output
		if ref, _ := schema["$ref"].(string); ref != "" {
			defs, _ := schema["$defs"].(map[string]any)
			schema, _ = defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		}
		props, _ := schema["properties"].(map[string]any)
		return sortedKeys(props)
	}
	t.Fatalf("spec has no output schema for %s", want)
	return nil
}

// TestDestructiveOnlyOnSocket: a destructive daemon command runs over
// the control socket, the operator's confirmation carried from the
// terminal; REST, gRPC and MCP refuse it.
func TestDestructiveOnlyOnSocket(t *testing.T) {
	withE2EBinary(t, func() {
		s := newSandbox(t)
		x := startSurfaces(t, s)
		defer func() { s.want(result{exit: x.d.term()}, 0) }()
		token := x.auth.token(me())

		// The terminal: kit's gate asks here; refused, nothing is sent.
		r := s.run(nil, "e2e", "destroy", "--format", "json")
		s.want(r, 5, "e2e", "destroy")
		r = s.run(nil, "e2e", "destroy", "--confirm=yes", "--format", "json")
		s.want(r, 0, "e2e", "destroy", "--confirm=yes")
		if !strings.Contains(r.stdout, `"actor": "`+me()+`"`) || !strings.Contains(r.stdout, `"surface": "socket"`) {
			t.Errorf("destroy over the socket = %s", r.stdout)
		}

		// REST: withheld from discovery and refused.
		code, doc := x.rest(t, http.MethodGet, "/v1/commands", token, "")
		if code != http.StatusOK {
			t.Fatalf("discovery = %d", code)
		}
		cmds, _ := doc["commands"].([]any)
		found := false
		for _, c := range cmds {
			e, _ := c.(map[string]any)
			if e["name"] == "e2e destroy" {
				found = true
				if e["invocable"] != false || e["reason"] != "unauthorized-destructive" {
					t.Errorf("REST discovery: %v", e)
				}
			}
		}
		if !found {
			t.Error("REST discovery does not list e2e destroy")
		}
		if code, body := x.rest(t, http.MethodPost, "/v1/commands/e2e/destroy", token, `{"flags":{"confirm":"yes"}}`); code < 400 {
			t.Errorf("REST ran a destructive command: %d %v", code, body)
		}

		// gRPC: refused before running.
		if code, body := x.invokeRPC(t, token, map[string]any{"path": []string{"e2e", "destroy"},
			"flags": map[string]any{"confirm": "yes"}}); code < 400 || !strings.Contains(strings.ToLower(asString(body)), "permission_denied") {
			t.Errorf("rpc destroy = %d %v", code, body)
		}

		// MCP: not a tool.
		m := newMCPClient(t, "http://"+x.mcp+"/mcp", token)
		if m.tools(t)["e2e.destroy"] {
			t.Error("MCP lists e2e.destroy")
		}
	})
}

func asString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestMCPStdioRelaysOverSocket: an MCP stdio process never becomes the
// daemon. It relays a daemon command over the control socket, where
// the daemon verifies the spawning user as the peer; with no daemon it
// refuses to start, exit 70.
func TestMCPStdioRelaysOverSocket(t *testing.T) {
	s := newSandbox(t)

	r := s.run(nil, "service", "start", "mcp", "--stdio")
	s.want(r, 70, "service", "start", "mcp", "--stdio")
	if !strings.Contains(r.stderr, "PREREQUISITE") || !strings.Contains(r.stderr, "heo service start") {
		t.Errorf("stderr = %s", r.stderr)
	}

	// The relay is not the daemon: it runs beside it, on a kit control
	// socket of its own, and leaves the daemon's alone.
	site := newCLISite(t, "")
	s = site.s
	d := site.start()
	defer site.stop(d)

	p := s.stdio("service", "start", "mcp", "--stdio")
	p.initialize()
	p.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"channel.list","arguments":{}}}`)
	reply := p.await(2)
	if st := s.serviceStatus(); len(st.Services) == 0 || st.Services[0].PID != d.cmd.Process.Pid {
		t.Errorf("with a relay running, service status = %+v, want the daemon (pid %d)", st.Services, d.cmd.Process.Pid)
	}
	p.stop()
	errLog := p.stderr()

	res, _ := reply["result"].(map[string]any)
	if res == nil || res["isError"] == true {
		t.Fatalf("relayed channel list failed: %v\n%s", reply, errLog)
	}
	for _, svc := range []string{"workers", "receivers", "socket"} {
		if regexpReady(svc).MatchString(errLog) {
			t.Errorf("the stdio process started %s:\n%s", svc, errLog)
		}
	}
	// The pidfile still names the daemon, not the relay.
	if b, err := os.ReadFile(s.path("run", "heo", "default", "heo.pid")); err != nil ||
		strings.TrimSpace(string(b)) != strconv.Itoa(d.cmd.Process.Pid) {
		t.Errorf("daemon pidfile = %q, %v; want pid %d", b, err, d.cmd.Process.Pid)
	}
	// The relay's control files go with it.
	if left, _ := filepath.Glob(s.path("run", "heo", "control-default.stdio-*")); len(left) > 0 {
		t.Errorf("relay control files left behind: %v", left)
	}
}

// TestDaemonRefusesUnverifiedCaller: inside the daemon a daemon command
// runs only for a caller the transport verified. MCP over stdio served
// by the daemon itself names no one, so the permission gate every
// surface shares refuses it.
func TestDaemonRefusesUnverifiedCaller(t *testing.T) {
	s := newSandbox(t)
	// One process serving the socket and MCP over stdio: the daemon.
	p := s.stdio("-c", "services.mcp.transport=stdio", "service", "start", noRuntime, "--enable", "mcp")
	p.initialize()
	p.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"channel.list","arguments":{}}}`)
	reply := p.await(2)
	p.stop()
	res, _ := reply["result"].(map[string]any)
	if res == nil || res["isError"] != true || !strings.Contains(asString(res), "heo.control.unverified_caller") {
		t.Errorf("unverified stdio channel list in the daemon = %v\n%s", reply, p.stderr())
	}
	if !regexpReady("socket").MatchString(p.stderr()) {
		t.Errorf("the process did not serve the socket:\n%s", p.stderr())
	}
}

// stdioProc is one `service start` process speaking MCP over stdio.
type stdioProc struct {
	t     *testing.T
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines *bufio.Scanner
	errb  lockedBuffer
}

// lockedBuffer is a bytes.Buffer safe to read while the process writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (s *sandbox) stdio(args ...string) *stdioProc {
	s.t.Helper()
	p := &stdioProc{t: s.t}
	p.cmd = exec.Command(heoBin, args...) //nolint:gosec // runs the binary TestMain built
	p.cmd.Dir = s.path("cwd")
	for k, v := range s.env {
		p.cmd.Env = append(p.cmd.Env, k+"="+v)
	}
	var err error
	if p.in, err = p.cmd.StdinPipe(); err != nil {
		s.t.Fatal(err)
	}
	out, err := p.cmd.StdoutPipe()
	if err != nil {
		s.t.Fatal(err)
	}
	p.cmd.Stderr = &p.errb
	if err := p.cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = p.cmd.Process.Kill(); _ = p.cmd.Wait() })
	p.lines = bufio.NewScanner(out)
	p.lines.Buffer(make([]byte, 0, 64*1024), 4<<20)
	return p
}

func (p *stdioProc) send(line string) {
	p.t.Helper()
	if _, err := io.WriteString(p.in, line+"\n"); err != nil {
		p.t.Fatal(err)
	}
}

// await reads stdout until the reply to id; the timer kills a process
// that never answers.
func (p *stdioProc) await(id int) map[string]any {
	p.t.Helper()
	timer := time.AfterFunc(runTimeout, func() { _ = p.cmd.Process.Kill() })
	defer timer.Stop()
	for p.lines.Scan() {
		var msg map[string]any
		if json.Unmarshal(p.lines.Bytes(), &msg) == nil && msg["id"] == float64(id) {
			return msg
		}
	}
	p.t.Fatalf("no reply %d:\n%s", id, p.errb.String())
	return nil
}

func (p *stdioProc) initialize() {
	p.t.Helper()
	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}`)
	p.await(1)
	p.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

// stop closes stdin and terminates the process.
func (p *stdioProc) stop() {
	_ = p.in.Close()
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
}

func (p *stdioProc) stderr() string { return p.errb.String() }

// TestServedCallOutsideDaemonRefused: a `service start` that does not
// serve the profile's control socket (here REST alone) is not the
// daemon. It refuses a daemon command with PREREQUISITE rather than run
// it without the daemon's runtime or re-send it under its own identity.
func TestServedCallOutsideDaemonRefused(t *testing.T) {
	s := newSandbox(t)
	auth := s.useHTTPAuth()
	api := freeAddr(t)
	lone := s.start(nil, nil, []string{"api"}, "-c", auth.config, "service", "start", "api", "--addr", api)
	defer func() { s.want(result{exit: lone.term()}, 0) }()
	resp, err := authed(http.MethodGet, "http://"+api+"/v1/commands/channel/list", auth.token(me()), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(b), `"exit_code":70`) ||
		!strings.Contains(string(b), "does not serve the profile's control socket") {
		t.Errorf("lone REST channel list = %d %s", resp.StatusCode, b)
	}
}
