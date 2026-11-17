//go:build unix

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/heo/internal/control"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/socket"
)

// serveEach runs a daemon on a short socket path that hands every
// connection to serve, and returns a client of it.
func serveEach(t *testing.T, serve func(net.Conn)) control.Client {
	t.Helper()
	base := os.TempDir()
	if len(base) > 32 {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "hw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory: 0700 is owner-only
		t.Fatal(err)
	}
	path := filepath.Join(dir, "heo.sock")
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
	return control.Client{Path: path, Start: "heo service start"}
}

func takeRequest(conn net.Conn) { _, _ = bufio.NewReader(conn).ReadBytes('\n') }

// closeAfterRequest is a daemon stopped while it held the call.
func closeAfterRequest(conn net.Conn) { takeRequest(conn) }

// cancelAfterRequest is a daemon that cancels the call as it stops.
func cancelAfterRequest(conn net.Conn) {
	takeRequest(conn)
	_, _ = conn.Write([]byte(`{"ok":false,"error":{"code":"INTERNAL","message":"cmdsurface: canceled: context canceled"}}` + "\n"))
}

// refuseAfterRequest is a daemon that answers with an error of its own.
func refuseAfterRequest(conn net.Conn) {
	takeRequest(conn)
	_, _ = conn.Write([]byte(`{"ok":false,"error":{"code":"INTERNAL","message":"store: disk full"}}` + "\n"))
}

// lostByClient is the error the control client returns for a call that
// serve dropped.
func lostByClient(t *testing.T, serve func(net.Conn)) error {
	t.Helper()
	_, err := serveEach(t, serve).Invoke(context.Background(), socket.Request{Path: []string{"task", "watch"}})
	if err == nil {
		t.Fatal("the call succeeded")
	}
	return err
}

// TestRetryClassifiesWhatTheClientReturns: the helper reads each kind of
// failure as the control client actually returns it. A daemon lost mid-
// call is waited for once the watch has attached, as a refusal for no
// daemon is, and ends it with exit 70 when the bound is 0; before it
// attaches either ends the watch at once; a daemon that answered with an
// error ends it, attached or not.
func TestRetryClassifiesWhatTheClientReturns(t *testing.T) {
	type outcome int
	const (
		ends     outcome = iota // the error ends the watch, as it is
		waits                   // retried, said once, then exit 70 if it never ends
		endsUnav                // ends the watch at once as PREREQUISITE
	)
	for _, tc := range []struct {
		name     string
		err      error
		attached outcome
		fresh    outcome
	}{
		{"no daemon", prerequisite(), waits, ends},
		{"connection closed mid-call", lostByClient(t, closeAfterRequest), waits, ends},
		{"call cancelled as the daemon stops", lostByClient(t, cancelAfterRequest), waits, ends},
		{"daemon answered with an error", lostByClient(t, refuseAfterRequest), ends, ends},
		{"not found", output.NotFoundError("no such operation"), ends, ends},
		{"plain error", errors.New("boom"), ends, ends},
	} {
		for _, attached := range []bool{true, false} {
			name := tc.name + "/fresh"
			want := tc.fresh
			if attached {
				name, want = tc.name+"/attached", tc.attached
			}
			t.Run(name, func(t *testing.T) {
				clk := newFakeClock()
				said := &bytes.Buffer{}
				var calls atomic.Int32
				run := retryOn(clk.clock(), func(context.Context, struct{}) (int, error) {
					if n := calls.Add(1); attached && n == 1 {
						return 1, nil // attached by this first call
					}
					return 0, tc.err
				}, 3*time.Second, said)
				if attached {
					if _, err := run(context.Background(), struct{}{}); err != nil {
						t.Fatal(err)
					}
					calls.Store(1)
				}
				before := calls.Load()
				_, err := run(context.Background(), struct{}{})
				tried := calls.Load() - before
				var oe *output.Error
				switch want {
				case ends:
					if err == nil || tried != 1 || clk.slept != 0 || said.Len() != 0 {
						t.Errorf("err %v after %d tries, waited %s, said %q: want it ended at once", err, tried, clk.slept, said)
					}
				case waits:
					if !errors.As(err, &oe) || oe.Code != output.CodePrerequisite || oe.ExitCode != 70 {
						t.Errorf("err = %v, want PREREQUISITE (exit 70) once the bound ran out", err)
					}
					if tried < 3 || clk.slept != 3*time.Second || strings.Count(said.String(), "waiting up to 3s") != 1 {
						t.Errorf("%d tries, waited %s, said %q: want retries for the 3s bound, said once", tried, clk.slept, said)
					}
				}
			})
		}
	}
}

// TestRetryRidesOutADaemonThatDropsCallsAndReturns: a daemon that drops
// each call for a while (closed, or cancelled as it stops) and then
// answers is carried through, with one line and no error.
func TestRetryRidesOutADaemonThatDropsCallsAndReturns(t *testing.T) {
	for name, drop := range map[string]func(net.Conn){"closed": closeAfterRequest, "cancelled": cancelAfterRequest} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			c := serveEach(t, func(conn net.Conn) {
				if calls.Add(1) <= 4 {
					drop(conn)
					return
				}
				takeRequest(conn)
				_, _ = conn.Write([]byte(`{"ok":true,"result":{"exit_code":0,"data":{}}}` + "\n"))
			})
			old := retryBase
			retryBase = time.Millisecond
			t.Cleanup(func() { retryBase = old })
			said := &bytes.Buffer{}
			run := retryTransient(func(ctx context.Context, _ struct{}) (control.Result, error) {
				return c.Invoke(ctx, socket.Request{Path: []string{"task", "watch"}})
			}, time.Minute, said)
			// Attached first, as a watch is by its open.
			calls.Store(4)
			if _, err := run(t.Context(), struct{}{}); err != nil {
				t.Fatal(err)
			}
			calls.Store(0)
			if _, err := run(t.Context(), struct{}{}); err != nil {
				t.Fatalf("err = %v, want the call carried through the outage", err)
			}
			if n := calls.Load(); n != 5 {
				t.Errorf("%d calls, want 4 dropped and 1 answered", n)
			}
			if got := said.String(); got != "daemon unavailable, waiting up to 1m0s\n" {
				t.Errorf("said %q, want the waiting line once", got)
			}
		})
	}
}
