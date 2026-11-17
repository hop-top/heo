//go:build unix

package control_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/cmdsurface"
	"hop.top/kit/go/transport/socket"
	"hop.top/uma/model"
)

// shortDir is an owner-only directory whose socket paths fit the
// 103-byte limit, which darwin's long temp dir would not.
func shortDir(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	if len(base) > 32 {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "hc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory: 0700 is owner-only
		t.Fatal(err)
	}
	return dir
}

// fakeDaemon answers every request line with answer and records what
// it received.
type fakeDaemon struct {
	ln       net.Listener
	got      chan socket.Request
	accepted atomic.Int32
}

func listen(t *testing.T, path string, answer func(socket.Request) string) *fakeDaemon {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDaemon{ln: ln, got: make(chan socket.Request, 8)}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.accepted.Add(1)
			go func() {
				defer func() { _ = conn.Close() }()
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var req socket.Request
				_ = json.Unmarshal(line, &req)
				f.got <- req
				_, _ = conn.Write([]byte(answer(req) + "\n"))
			}()
		}
	}()
	return f
}

func asCLI(t *testing.T, err error) *output.Error {
	t.Helper()
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("error %T %v is not a kit structured error", err, err)
	}
	return oe
}

// TestDaemonAbsentIsPrerequisite: no directory, no socket, or a socket
// nothing listens on all answer PREREQUISITE, exit 70, naming the
// command that starts the daemon; nothing is created.
func TestDaemonAbsentIsPrerequisite(t *testing.T) {
	dir := shortDir(t)
	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	for name, path := range map[string]string{
		"no dir":    filepath.Join(dir, "missing", "heo.sock"),
		"no socket": filepath.Join(dir, "heo.sock"),
		"stale":     stale,
	} {
		c := control.Client{Path: path, Start: "heo --profile work service start"}
		_, err := c.Invoke(context.Background(), socket.Request{Path: []string{"daemon", "ping"}})
		e := asCLI(t, err)
		if e.Code != output.CodePrerequisite || e.ExitCode != 70 {
			t.Errorf("%s: %+v, want PREREQUISITE/70", name, e)
		}
		if !strings.Contains(e.SuggestedFix, "heo --profile work service start") {
			t.Errorf("%s: fix %q does not name the start command", name, e.SuggestedFix)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the client created the socket directory: %v", err)
	}
}

// TestForeignSocketRefusedBeforeSending: a socket in a directory
// another user owns, or one other users can enter, is refused with
// UNAUTHORIZED before anything is sent to whoever listens there.
func TestForeignSocketRefusedBeforeSending(t *testing.T) {
	t.Run("open directory", func(t *testing.T) {
		dir := shortDir(t)
		path := filepath.Join(dir, "heo.sock")
		squatter := listen(t, path, func(socket.Request) string { return `{"ok":true,"result":{"exit_code":0}}` })
		if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the squatted directory under test
			t.Fatal(err)
		}
		_, err := control.Client{Path: path}.Invoke(context.Background(), socket.Request{Path: []string{"daemon", "ping"}})
		e := asCLI(t, err)
		if e.Code != output.CodeUnauthorized || e.ExitCode != 5 || !strings.Contains(e.Message, "open to other users") {
			t.Errorf("open dir: %+v", e)
		}
		time.Sleep(50 * time.Millisecond)
		if n := squatter.accepted.Load(); n != 0 {
			t.Errorf("the client connected to the squatter %d times", n)
		}
	})
	t.Run("directory of another user", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root owns /")
		}
		_, err := control.Client{Path: "/heo.sock"}.Invoke(context.Background(), socket.Request{Path: []string{"daemon", "ping"}})
		e := asCLI(t, err)
		if e.Code != output.CodeUnauthorized || !strings.Contains(e.Message, "owned by uid 0") {
			t.Errorf("root-owned dir: %+v", e)
		}
	})
	t.Run("not a socket", func(t *testing.T) {
		dir := shortDir(t)
		path := filepath.Join(dir, "heo.sock")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := control.Client{Path: path}.Invoke(context.Background(), socket.Request{Path: []string{"x"}})
		if e := asCLI(t, err); e.Code != output.CodeUnauthorized || !strings.Contains(e.Message, "not a socket") {
			t.Errorf("regular file: %+v", e)
		}
	})
	t.Run("symlinked directory", func(t *testing.T) {
		dir := shortDir(t)
		real := filepath.Join(dir, "real")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		listen(t, filepath.Join(real, "heo.sock"), func(socket.Request) string { return `{"ok":true,"result":{"exit_code":0}}` })
		link := filepath.Join(dir, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		_, err := control.Client{Path: filepath.Join(link, "heo.sock")}.Invoke(context.Background(), socket.Request{Path: []string{"x"}})
		if e := asCLI(t, err); e.Code != output.CodeUnauthorized {
			t.Errorf("symlinked dir: %+v", e)
		}
	})
}

// TestInvokeSendsNoIdentity: the request carries no caller and no
// tenant, whatever the caller set; the result comes back as sent.
func TestInvokeSendsNoIdentity(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "heo.sock")
	d := listen(t, path, func(socket.Request) string {
		return `{"ok":true,"result":{"exit_code":0,"data":{"n":12345678901234567890}}}`
	})
	res, err := control.Client{Path: path}.Invoke(context.Background(), socket.Request{
		Path: []string{"daemon", "ping"}, Caller: "uid:0", Tenant: "other",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := <-d.got
	if got.Caller != "" || got.Tenant != "" {
		t.Errorf("sent caller %q tenant %q", got.Caller, got.Tenant)
	}
	if string(res.Data) != `{"n":12345678901234567890}` {
		t.Errorf("data = %s, want the number intact", res.Data)
	}
}

// TestInvokeWireRefusal: a refusal the daemon answers is a kit error
// of its class, not a result.
func TestInvokeWireRefusal(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "heo.sock")
	listen(t, path, func(socket.Request) string {
		return `{"ok":false,"error":{"code":"BLOCKED","message":"destructive"}}`
	})
	_, err := control.Client{Path: path}.Invoke(context.Background(), socket.Request{Path: []string{"x"}})
	if e := asCLI(t, err); e.Code != output.CodeUnauthorized || e.ExitCode != 5 || e.Message != "destructive" {
		t.Errorf("BLOCKED → %+v", e)
	}
}

// TestDecodeKeepsEnvelope: a command that failed in the daemon comes
// back as the envelope it rendered, every field intact.
func TestDecodeKeepsEnvelope(t *testing.T) {
	stderr := `{
  "code": "OUTCOME_UNKNOWN",
  "message": "provider timed out",
  "cause": "correlation=c1",
  "suggested_fix": "reconcile first",
  "exit_code": 8,
  "transience": "unknown"
}
`
	_, err := control.Decode[map[string]any](control.Result{ExitCode: 8, Stderr: stderr})
	e := asCLI(t, err)
	want := output.Error{Code: "OUTCOME_UNKNOWN", Message: "provider timed out", Cause: "correlation=c1",
		SuggestedFix: "reconcile first", ExitCode: 8, Transience: "unknown"}
	if e.Code != want.Code || e.Message != want.Message || e.Cause != want.Cause ||
		e.SuggestedFix != want.SuggestedFix || e.ExitCode != want.ExitCode || e.Transience != want.Transience {
		t.Errorf("envelope = %+v, want %+v", e, want)
	}
	_, err = control.Decode[map[string]any](control.Result{ExitCode: 3, Stderr: "plain text"})
	if e := asCLI(t, err); e.ExitCode != 3 || e.Message != "plain text" {
		t.Errorf("plain stderr → %+v", e)
	}
	v, err := control.Decode[struct{ A int }](control.Result{Stdout: `{"A":7}`})
	if err != nil || v.A != 7 {
		t.Errorf("stdout fallback = %+v, %v", v, err)
	}
}

// TestDecodeReportKeepsAFailedReport: a command that renders its report
// and then fails with the report's own code comes back as the report,
// for the client to render and judge; any other failure stays the
// error, report or not.
func TestDecodeReportKeepsAFailedReport(t *testing.T) {
	failed := `{"code":"VALIDATION","message":"1 channel failed its check","exit_code":2}`
	v, err := control.DecodeReport[struct{ OK bool }](control.Result{
		ExitCode: 2, Stderr: failed, Data: []byte(`{"OK":false}`),
	}, "VALIDATION")
	if err != nil || v.OK {
		t.Fatalf("failed report = %+v, %v; want the report", v, err)
	}
	_, err = control.DecodeReport[struct{ OK bool }](control.Result{
		ExitCode: 5, Stderr: `{"code":"UNAUTHORIZED","message":"no","exit_code":5}`, Data: []byte(`{"OK":false}`),
	}, "VALIDATION")
	if e := asCLI(t, err); e.Code != "UNAUTHORIZED" || e.ExitCode != 5 {
		t.Errorf("another failure = %+v, want the envelope", e)
	}
	_, err = control.DecodeReport[struct{ OK bool }](control.Result{ExitCode: 2, Stderr: failed}, "VALIDATION")
	if e := asCLI(t, err); e.Code != "VALIDATION" {
		t.Errorf("failure without a report = %+v, want the envelope", e)
	}
}

// TestRequestFor: the command's path below the root, its own flags as
// set, json for the answer, and a destructive command's confirmation. A
// string array travels as its values, each whole: one holding a comma or
// a quote (a JSON value) reaches the daemon unsplit. A string slice, which
// the daemon reads back as CSV, travels as CSV of its values: empty ones
// and ones holding a comma survive. A bool set false
// travels as false, not dropped, and a repeated flag set to nothing as "".
func TestRequestFor(t *testing.T) {
	root := &cobra.Command{Use: "heo"}
	root.PersistentFlags().String("profile", "", "")
	root.PersistentFlags().Bool("dry-run", false, "")
	grp := &cobra.Command{Use: "thing"}
	leaf := &cobra.Command{Use: "drop", RunE: func(*cobra.Command, []string) error { return nil }}
	leaf.Flags().String("name", "", "")
	leaf.Flags().StringSlice("tag", nil, "")
	leaf.Flags().StringArray("field", nil, "")
	leaf.Flags().StringSlice("phone", nil, "")
	leaf.Flags().StringSlice("label", nil, "")
	leaf.Flags().StringSlice("quoted", nil, "")
	leaf.Flags().Bool("force", false, "")
	leaf.Flags().Bool("open", true, "")
	leaf.Flags().Int("limit", 10, "")
	cli.SetSideEffect(leaf, cli.SideEffectDestructive)
	root.AddCommand(grp)
	grp.AddCommand(leaf)
	root.SetArgs([]string{"--profile", "work", "thing", "drop", "--name", "x", "--tag", "a,b", "--field", `labels=["bug","ui"]`, "--field", "milestone=3", "--force", "--open=false", "--phone=", "--label", ",", "--quoted", `"a,b",c`, "--dry-run", "arg1"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	req := control.RequestFor(leaf, []string{"arg1"})
	if strings.Join(req.Path, " ") != "thing drop" || strings.Join(req.Args, ",") != "arg1" {
		t.Errorf("path %v args %v", req.Path, req.Args)
	}
	want := map[string]any{"name": "x", "tag": "a,b", "label": ",", "quoted": `"a,b",c`, "field": []string{`labels=["bug","ui"]`, "milestone=3"},
		"force": "true", "open": "false", "phone": "", "format": "json", "dry-run": true, "confirm": "yes"}
	if len(req.Flags) != len(want) {
		t.Errorf("flags = %v, want %v", req.Flags, want)
	}
	for k, v := range want {
		if !reflect.DeepEqual(req.Flags[k], v) {
			t.Errorf("flag %s = %#v, want %#v", k, req.Flags[k], v)
		}
	}
	read := &cobra.Command{Use: "list", RunE: func(*cobra.Command, []string) error { return nil }}
	cli.SetSideEffect(read, cli.SideEffectRead)
	grp.AddCommand(read)
	if r := control.RequestFor(read, nil); r.Flags["confirm"] != nil || r.Args == nil {
		t.Errorf("read request = %+v", r)
	}
}

// TestActorOnlyFromVerifiedCaller: an actor exists only for a caller
// the transport verified; a claim, or a transport-established caller
// with a name it never verified, is UNAUTHORIZED.
func TestActorOnlyFromVerifiedCaller(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta cmdsurface.Meta
		want model.ActorID
	}{
		{"socket peer", cmdsurface.Meta{Caller: "uid:501", Established: cmdsurface.EstablishedVerified, Surface: cmdsurface.SurfaceSocket}, "uid:501"},
		{"token subject", cmdsurface.Meta{Caller: "uid:501", Established: cmdsurface.EstablishedVerified, Surface: cmdsurface.SurfaceREST}, "uid:501"},
		{"claim only", cmdsurface.Meta{Caller: "uid:0", Surface: cmdsurface.SurfaceSocket}, ""},
		{"transport, named by claim", cmdsurface.Meta{Caller: "uid:0", Established: cmdsurface.EstablishedTransport, Surface: cmdsurface.SurfaceSocket}, ""},
		{"stdio spawn", cmdsurface.Meta{Established: cmdsurface.EstablishedTransport, Surface: cmdsurface.SurfaceMCP}, ""},
		{"verified, no name", cmdsurface.Meta{Established: cmdsurface.EstablishedVerified, Surface: cmdsurface.SurfaceREST}, ""},
	} {
		got, err := control.Actor(tc.meta)
		if got != tc.want {
			t.Errorf("%s: actor %q, want %q", tc.name, got, tc.want)
		}
		if tc.want == "" {
			if e := asCLI(t, err); e.Code != output.CodeUnauthorized || e.ExitCode != 5 {
				t.Errorf("%s: %+v, want UNAUTHORIZED/5", tc.name, e)
			}
		}
	}
	c, err := control.CallerOf(cmdsurface.Meta{Caller: "uid:501", Tenant: "claimed",
		Established: cmdsurface.EstablishedVerified, Surface: cmdsurface.SurfaceSocket}, "default")
	if err != nil || c.Tenant != "default" || c.Principal() != (model.Principal{Tenant: "default", Actor: "uid:501"}) {
		t.Errorf("caller = %+v, %v; want the daemon's tenant", c, err)
	}
}

// TestGateSameOnEverySurface: the gate decides a daemon command the
// same way whichever surface carries it — a verified caller the policy
// admits passes, one it does not is refused with the policy's reason,
// a claimed one is refused unverified — and leaves other commands to
// the deciders after it.
func TestGateSameOnEverySurface(t *testing.T) {
	daemonLeaf := &cobra.Command{Use: "ping"}
	control.MarkDaemon(daemonLeaf)
	other := &cobra.Command{Use: "list"}
	admit := func(actor model.ActorID) (bool, string) {
		if actor == "uid:501" {
			return true, ""
		}
		return false, "heo.authz.no_role"
	}
	for _, s := range []cmdsurface.Surface{cmdsurface.SurfaceSocket, cmdsurface.SurfaceREST, cmdsurface.SurfaceRPC, cmdsurface.SurfaceMCP} {
		gate := control.Gate(func() bool { return true }, admit)
		verified := cmdsurface.Meta{Caller: "uid:501", Established: cmdsurface.EstablishedVerified, Surface: s}
		stranger := cmdsurface.Meta{Caller: "uid:4242", Established: cmdsurface.EstablishedVerified, Surface: s}
		claimed := cmdsurface.Meta{Caller: "uid:501", Surface: s}
		if d := gate(context.Background(), verified, &cmdsurface.Leaf{Cmd: daemonLeaf}); !d.Allowed {
			t.Errorf("%s: verified caller denied: %s", s, d.Reason)
		}
		if d := gate(context.Background(), stranger, &cmdsurface.Leaf{Cmd: daemonLeaf}); d.Allowed || d.Reason != "heo.authz.no_role" || d.CallerIndependent {
			t.Errorf("%s: verified caller the policy does not admit = %+v", s, d)
		}
		if d := gate(context.Background(), claimed, &cmdsurface.Leaf{Cmd: daemonLeaf}); d.Allowed || d.Reason != control.ReasonUnverified || d.CallerIndependent {
			t.Errorf("%s: claimed caller = %+v", s, d)
		}
		if d := gate(context.Background(), claimed, &cmdsurface.Leaf{Cmd: other}); !d.Allowed {
			t.Errorf("%s: a non-daemon command was gated", s)
		}
	}
	stdio := cmdsurface.Meta{Established: cmdsurface.EstablishedTransport, Surface: cmdsurface.SurfaceMCP}
	if d := control.Gate(func() bool { return false }, admit)(context.Background(), stdio, &cmdsurface.Leaf{Cmd: daemonLeaf}); !d.Allowed {
		t.Error("an MCP stdio relay was denied")
	}
	if d := control.Gate(func() bool { return true }, admit)(context.Background(), stdio, &cmdsurface.Leaf{Cmd: daemonLeaf}); d.Allowed {
		t.Error("an unnamed MCP stdio caller was admitted in the daemon itself")
	}
}

// TestDecide: the terminal forwards; the daemon runs served calls; a
// stdio MCP process relays; any other served call elsewhere is refused.
func TestDecide(t *testing.T) {
	verifiedREST := cmdsurface.Meta{Caller: "uid:1", Established: cmdsurface.EstablishedVerified, Surface: cmdsurface.SurfaceREST}
	stdio := cmdsurface.Meta{Established: cmdsurface.EstablishedTransport, Surface: cmdsurface.SurfaceMCP}
	for _, tc := range []struct {
		name          string
		meta          cmdsurface.Meta
		served, serve bool
		want          control.Route
	}{
		{"terminal", cmdsurface.Meta{}, false, false, control.Forward},
		{"daemon, socket call", cmdsurface.Meta{Surface: cmdsurface.SurfaceSocket}, true, true, control.Local},
		{"daemon, REST call", verifiedREST, true, true, control.Local},
		{"stdio relay", stdio, true, false, control.Forward},
		{"REST outside the daemon", verifiedREST, true, false, control.Refuse},
	} {
		if got := control.Decide(tc.meta, tc.served, tc.serve); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSocketPolicyNamesSocketOnly: the socket's ceiling admits a
// destructive command; every other surface keeps refusing it, and so
// does kit's zero policy on the socket.
func TestSocketPolicyNamesSocketOnly(t *testing.T) {
	destructive := cmdsurface.SafetyClass{Destructive: true}
	p := control.SocketPolicy()
	if !p.Allowed(destructive, cmdsurface.SurfaceSocket) {
		t.Error("socket refuses destructive commands")
	}
	for _, s := range []cmdsurface.Surface{cmdsurface.SurfaceREST, cmdsurface.SurfaceRPC, cmdsurface.SurfaceMCP} {
		if p.Allowed(destructive, s) {
			t.Errorf("%s admits destructive commands", s)
		}
	}
	if (cmdsurface.Policy{}).Allowed(destructive, cmdsurface.SurfaceSocket) {
		t.Error("kit's zero policy admits destructive commands on the socket")
	}
}

// TestGateRefusesPluginOnSocket: a plugin actor is never admitted on
// the control socket, whatever the policy says; an HTTP surface that
// verified its token admits it.
func TestGateRefusesPluginOnSocket(t *testing.T) {
	daemonLeaf := &cobra.Command{Use: "ping"}
	control.MarkDaemon(daemonLeaf)
	admit := func(model.ActorID) (bool, string) { return true, "" }
	gate := control.Gate(func() bool { return true }, admit)
	meta := func(s cmdsurface.Surface) cmdsurface.Meta {
		return cmdsurface.Meta{Caller: "plugin:triage", Established: cmdsurface.EstablishedVerified, Surface: s}
	}
	if d := gate(context.Background(), meta(cmdsurface.SurfaceSocket), &cmdsurface.Leaf{Cmd: daemonLeaf}); d.Allowed || d.Reason != control.ReasonPluginOnSocket {
		t.Errorf("plugin on socket = %+v", d)
	}
	for _, s := range []cmdsurface.Surface{cmdsurface.SurfaceREST, cmdsurface.SurfaceRPC, cmdsurface.SurfaceMCP} {
		if d := gate(context.Background(), meta(s), &cmdsurface.Leaf{Cmd: daemonLeaf}); !d.Allowed {
			t.Errorf("%s: plugin with verified token denied: %s", s, d.Reason)
		}
	}
}
