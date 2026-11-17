package hooks_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/hooks"
)

// sockServer is a fixture plugin server on a unix socket. handle gets
// each envelope line and returns the bytes to write back, or nil for
// nothing.
type sockServer struct {
	path string
	ln   net.Listener
	mu   sync.Mutex
	conn []net.Conn
	seen []string
	h    func(env hooks.Envelope) []byte
}

func sockPath(t *testing.T) string {
	t.Helper()
	// Short: a unix socket path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("/tmp", "heo-sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "p.sock")
}

func echo(env hooks.Envelope) []byte {
	return []byte(`{"event_id":"` + env.EventID + `","operations":[]}` + "\n")
}

func startSock(t *testing.T, path string, h func(hooks.Envelope) []byte) *sockServer {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	s := &sockServer{path: path, ln: ln, h: h}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conn = append(s.conn, c)
			s.mu.Unlock()
			go func() {
				sc := bufio.NewScanner(c)
				sc.Buffer(nil, 4<<20)
				for sc.Scan() {
					var env hooks.Envelope
					if json.Unmarshal(sc.Bytes(), &env) != nil {
						return
					}
					s.mu.Lock()
					s.seen = append(s.seen, env.EventID)
					s.mu.Unlock()
					if out := s.h(env); out != nil {
						_, _ = c.Write(out)
					}
				}
			}()
		}
	}()
	t.Cleanup(s.stop)
	return s
}

// stop closes the listener and every connection, as a restart does.
func (s *sockServer) stop() {
	_ = s.ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conn {
		_ = c.Close()
	}
	s.conn = nil
	_ = os.Remove(s.path)
}

func (s *sockServer) saw() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func TestSocketProcess(t *testing.T) {
	path := sockPath(t)
	srv := startSock(t, path, echo)
	p := &hooks.SocketProcess{Path: path, Timeout: 5 * time.Second}
	defer func() { _ = p.Close() }() // teardown
	for _, id := range []string{"e1", "e2", "e3"} {
		res, err := p.Run(context.Background(), env(id))
		if err != nil || res.EventID != id {
			t.Fatalf("%s: %+v %v", id, res, err)
		}
	}
	if got := srv.saw(); strings.Join(got, ",") != "e1,e2,e3" {
		t.Errorf("server saw %v", got)
	}
}

// TestSocketProcessAbsent: no socket is a failed attempt, not a crash,
// and not permanent; the plugin may start later.
func TestSocketProcessAbsent(t *testing.T) {
	p := &hooks.SocketProcess{Path: sockPath(t), Timeout: time.Second}
	_, err := p.Run(context.Background(), env("e1"))
	if err == nil || hooks.IsPermanent(err) || !strings.Contains(err.Error(), "connect") {
		t.Errorf("err %v", err)
	}
}

// TestSocketProcessRestart: the server restarts between and under
// envelopes; the failed attempt is retryable and the next one
// reconnects.
func TestSocketProcessRestart(t *testing.T) {
	path := sockPath(t)
	srv := startSock(t, path, echo)
	p := &hooks.SocketProcess{Path: path, Timeout: 2 * time.Second}
	defer func() { _ = p.Close() }() // teardown
	if _, err := p.Run(context.Background(), env("e1")); err != nil {
		t.Fatal(err)
	}
	srv.stop()
	if _, err := p.Run(context.Background(), env("e2")); err == nil || hooks.IsPermanent(err) {
		t.Errorf("down: err %v", err)
	}
	startSock(t, path, echo)
	if res, err := p.Run(context.Background(), env("e2")); err != nil || res.EventID != "e2" {
		t.Errorf("restarted: %+v %v", res, err)
	}
}

// TestSocketProcessClosedUnderEnvelope: a server that hangs up instead
// of answering fails that attempt.
func TestSocketProcessClosedUnderEnvelope(t *testing.T) {
	path := sockPath(t)
	var srv *sockServer
	srv = startSock(t, path, func(hooks.Envelope) []byte {
		go srv.stop()
		return nil
	})
	p := &hooks.SocketProcess{Path: path, Timeout: 2 * time.Second}
	defer func() { _ = p.Close() }() // teardown
	_, err := p.Run(context.Background(), env("e1"))
	if err == nil || hooks.IsPermanent(err) || hooks.Classify(err) != hooks.ClassCrash {
		t.Errorf("err %v class %v", err, hooks.Classify(err))
	}
}

func TestSocketProcessOversize(t *testing.T) {
	path := sockPath(t)
	startSock(t, path, func(e hooks.Envelope) []byte {
		return []byte(`{"event_id":"` + e.EventID + `","pad":"` + strings.Repeat("x", 5000) + `","operations":[]}` + "\n")
	})
	p := &hooks.SocketProcess{Path: path, Timeout: 2 * time.Second, Launch: hooks.Launch{MaxOutput: 1000}}
	defer func() { _ = p.Close() }() // teardown
	_, err := p.Run(context.Background(), env("e1"))
	if err == nil || hooks.IsPermanent(err) || hooks.Classify(err) != hooks.ClassMalformed || !strings.Contains(err.Error(), "larger than 1000") {
		t.Errorf("err %v", err)
	}
}

func TestSocketProcessTimeout(t *testing.T) {
	path := sockPath(t)
	startSock(t, path, func(hooks.Envelope) []byte { return nil })
	p := &hooks.SocketProcess{Path: path, Timeout: 100 * time.Millisecond}
	defer func() { _ = p.Close() }() // teardown
	start := time.Now()
	_, err := p.Run(context.Background(), env("e1"))
	if err == nil || hooks.IsPermanent(err) || hooks.Classify(err) != hooks.ClassTimeout {
		t.Errorf("err %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

// TestSocketProcessLateAnswer: an answer to an event given up on is
// skipped, not taken for the next one's.
func TestSocketProcessLateAnswer(t *testing.T) {
	path := sockPath(t)
	startSock(t, path, func(e hooks.Envelope) []byte {
		return []byte(`{"event_id":"stale","operations":[]}` + "\n" + string(echo(e)))
	})
	p := &hooks.SocketProcess{Path: path, Timeout: 2 * time.Second}
	defer func() { _ = p.Close() }() // teardown
	if res, err := p.Run(context.Background(), env("e1")); err != nil || res.EventID != "e1" {
		t.Errorf("%+v %v", res, err)
	}
}

// TestSocketProcessRefusesUnsafeSocket: a path that is not a socket, or
// that every user may write, is refused.
func TestSocketProcessRefusesUnsafeSocket(t *testing.T) {
	path := sockPath(t)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := &hooks.SocketProcess{Path: path, Timeout: time.Second}
	if _, err := p.Run(context.Background(), env("e1")); err == nil || !strings.Contains(err.Error(), "not a unix socket") {
		t.Errorf("file: %v", err)
	}
	_ = os.Remove(path) // may already be gone
	srv := startSock(t, path, echo)
	_ = srv
	if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec // the test makes the socket world-writable on purpose
		t.Fatal(err)
	}
	if _, err := p.Run(context.Background(), env("e1")); err == nil || !strings.Contains(err.Error(), "writable by all") {
		t.Errorf("world-writable: %v", err)
	}
	if got := srv.saw(); len(got) != 0 {
		t.Errorf("server was sent %v", got)
	}
}

// TestSocketAbsentRetriesThenDeadLetters: through the runner, an absent
// socket is attempts under the plugin's backoff, then a dead letter.
func TestSocketAbsentRetriesThenDeadLetters(t *testing.T) {
	ex := &hooks.SocketProcess{Path: sockPath(t), Timeout: time.Second}
	var mu sync.Mutex
	var dead []hooks.DeadLetter
	run(t, hooks.Options{
		Plugin: plugin("p", 1, 3), Source: newLog(event("e1", "c1")), Exec: ex,
		Dead: fnDead(func(_ context.Context, d hooks.DeadLetter) error {
			mu.Lock()
			defer mu.Unlock()
			dead = append(dead, d)
			return nil
		}),
	})
	eventually(t, "dead letter", func() bool { mu.Lock(); defer mu.Unlock(); return len(dead) == 1 })
	mu.Lock()
	defer mu.Unlock()
	if dead[0].Attempts != 3 || dead[0].Class != hooks.ClassError || dead[0].Permanent {
		t.Errorf("dead %+v", dead[0])
	}
}
