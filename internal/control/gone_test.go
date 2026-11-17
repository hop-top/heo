//go:build unix

package control_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/heo/internal/control"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/socket"
)

// serving runs a daemon that hands each connection to serve, and
// returns a client of it.
func serving(t *testing.T, serve func(net.Conn)) control.Client {
	t.Helper()
	path := filepath.Join(shortDir(t), "heo.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				serve(conn)
			}()
		}
	}()
	return control.Client{Path: path, Start: "heo --profile work service start"}
}

// readRequest reads the request line, as a daemon that took the call.
func readRequest(conn net.Conn) { _, _ = bufio.NewReader(conn).ReadBytes('\n') }

// TestInvokeDaemonLostMidCallIsGone: a daemon that takes the call and
// then closes the connection, cuts its answer short, or reports the call
// cancelled because it is stopping, has stopped answering: control.Gone.
// The error stays an unknown-outcome failure for a command that may have
// run, and renders as PREREQUISITE (exit 70) naming the command that
// starts a daemon where a caller waits for one.
func TestInvokeDaemonLostMidCallIsGone(t *testing.T) {
	for name, tc := range map[string]struct {
		serve func(net.Conn)
		msg   string
	}{
		"closed after the request": {
			serve: readRequest,
			msg:   "the daemon closed the connection before answering",
		},
		"closed before the request": {
			serve: func(net.Conn) {},
			msg:   "", // EOF, a reset or a broken pipe: whichever the write or read meets
		},
		"answer cut short": {
			serve: func(conn net.Conn) { readRequest(conn); _, _ = conn.Write([]byte(`{"ok":tr`)) },
			msg:   "the daemon closed the connection before answering",
		},
		"call cancelled as the daemon stops": {
			serve: func(conn net.Conn) {
				readRequest(conn)
				_, _ = conn.Write([]byte(`{"ok":false,"error":{"code":"INTERNAL","message":"cmdsurface: canceled: context canceled"}}` + "\n"))
			},
			msg: "cmdsurface: canceled: context canceled",
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := serving(t, tc.serve)
			_, err := c.Invoke(context.Background(), socket.Request{Path: []string{"task", "watch"}})
			if err == nil || !control.Gone(err) {
				t.Fatalf("err = %v, want a lost daemon (control.Gone)", err)
			}
			e := asCLI(t, err)
			if e.Code != output.CodeGeneric || e.Transience != output.TransienceUnknown {
				t.Errorf("err = %+v, want GENERIC with an unknown outcome: the command may have run", e)
			}
			if tc.msg != "" && !strings.Contains(e.Message, tc.msg) {
				t.Errorf("message %q lacks %q", e.Message, tc.msg)
			}
			u := control.Unavailable(err)
			if u.Code != output.CodePrerequisite || u.ExitCode != 70 || u.Transience != output.TransienceTransient {
				t.Errorf("Unavailable = %+v, want PREREQUISITE, exit 70, transient", u)
			}
			if !strings.Contains(u.SuggestedFix, "heo --profile work service start") {
				t.Errorf("fix %q does not name the start command", u.SuggestedFix)
			}
		})
	}
}

// TestInvokeDaemonAnswerIsNotGone: a daemon that answers, even with an
// error, has not stopped answering: its refusal is the daemon's word, and
// only a cancellation it reports is its own stopping. A cancelled caller
// is not a lost daemon either.
func TestInvokeDaemonAnswerIsNotGone(t *testing.T) {
	answer := func(line string) func(net.Conn) {
		return func(conn net.Conn) { readRequest(conn); _, _ = conn.Write([]byte(line + "\n")) }
	}
	for name, serve := range map[string]func(net.Conn){
		"internal error":     answer(`{"ok":false,"error":{"code":"INTERNAL","message":"store: disk full"}}`),
		"rate limited":       answer(`{"ok":false,"error":{"code":"RATE_LIMITED","message":"slow down"}}`),
		"deadline exceeded":  answer(`{"ok":false,"error":{"code":"DEADLINE_EXCEEDED","message":"canceled: context canceled"}}`),
		"not json":           answer(`not json`),
		"neither result/err": answer(`{"ok":true}`),
	} {
		t.Run(name, func(t *testing.T) {
			c := serving(t, serve)
			_, err := c.Invoke(context.Background(), socket.Request{Path: []string{"task", "watch"}})
			if err == nil {
				t.Fatal("err = nil, want the daemon's refusal")
			}
			if control.Gone(err) {
				t.Errorf("err = %v counted as a lost daemon", err)
			}
		})
	}

	t.Run("caller gives up", func(t *testing.T) {
		c := serving(t, func(conn net.Conn) { readRequest(conn); select {} })
		ctx, cancel := context.WithCancel(context.Background())
		go func() { cancel() }()
		_, err := c.Invoke(ctx, socket.Request{Path: []string{"task", "watch"}})
		if err == nil || control.Gone(err) {
			t.Errorf("err = %v, want the caller's cancellation, not a lost daemon", err)
		}
	})

	t.Run("no daemon", func(t *testing.T) {
		c := control.Client{Path: filepath.Join(shortDir(t), "heo.sock")}
		_, err := c.Invoke(context.Background(), socket.Request{Path: []string{"task", "watch"}})
		if err == nil || control.Gone(err) {
			t.Errorf("err = %v: no daemon is PREREQUISITE already, not a lost one", err)
		}
	})

	if control.Gone(errors.New("boom")) || control.Gone(nil) {
		t.Error("an error that is not the client's counted as a lost daemon")
	}
}
