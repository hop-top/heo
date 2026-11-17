package hooks

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// dialTimeout bounds one connect to a plugin's socket.
const dialTimeout = 5 * time.Second

// SocketProcess delivers to a plugin that is already running as a
// server on a unix socket. It speaks the stream framing: one envelope
// line out, one result line back carrying the envelope's event_id. It
// is sequential, and starts nothing: the operator runs the plugin and
// gives it its token; no environment crosses a socket.
//
// A connection that is absent, refused or closed fails the attempt; the
// runner retries it under the plugin's backoff, and the next attempt
// connects again.
type SocketProcess struct {
	Path    string
	Timeout time.Duration
	// Launch supplies the plugin's name and result cap (MaxOutput).
	Launch Launch
	// DialTimeout bounds a connect; zero takes five seconds.
	DialTimeout time.Duration

	mu   sync.Mutex
	conn *socketConn
}

type socketConn struct {
	c        net.Conn
	lines    chan []byte
	overflow atomic.Bool
}

// Run implements Executor.
func (s *SocketProcess) Run(ctx context.Context, env Envelope) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line, err := json.Marshal(env)
	if err != nil {
		return Result{}, Permanent(err)
	}
	if s.conn == nil {
		c, err := s.dial(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			return Result{}, &Failure{Class: ClassError, Reason: "connect: " + err.Error()}
		}
		s.conn = c
	}
	p := s.conn
	_ = p.c.SetWriteDeadline(time.Now().Add(s.Timeout))
	if _, err := p.c.Write(append(line, '\n')); err != nil {
		s.drop()
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, &Failure{Class: ClassCrash, Reason: "write: " + err.Error()}
	}
	res, closed, err := awaitResult(ctx, env.EventID, p.lines, &p.overflow, s.Timeout, s.Launch.limit())
	switch {
	case closed:
		s.drop()
		return Result{}, &Failure{Class: ClassCrash, Reason: "plugin closed the connection"}
	case err != nil:
		s.drop()
		return Result{}, err
	}
	return res, nil
}

func (s *SocketProcess) dial(ctx context.Context) (*socketConn, error) {
	if err := checkSocket(s.Path); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: cmpDuration(s.DialTimeout, dialTimeout)}
	c, err := d.DialContext(ctx, "unix", s.Path)
	if err != nil {
		return nil, err
	}
	p := &socketConn{c: c, lines: make(chan []byte)}
	limit := s.Launch.limit()
	go func() {
		defer close(p.lines)
		r := bufio.NewReaderSize(c, 64<<10)
		for {
			b, err := readLine(r, limit)
			if len(b) > 0 {
				p.lines <- b
			}
			if errors.Is(err, errLineTooLong) {
				p.overflow.Store(true)
			}
			if err != nil {
				return
			}
		}
	}()
	return p, nil
}

func cmpDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// checkSocket refuses a path that is not a socket the daemon's own user
// owns, or that other users may write to: it would let another user
// stand in for the plugin and read the events.
func checkSocket(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a unix socket", path)
	}
	if err := checkSocketOwner(path, fi); err != nil {
		return err
	}
	if fi.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("%s is writable by all users (mode %04o)", path, fi.Mode().Perm())
	}
	return nil
}

// drop closes and forgets the connection; the next envelope dials again.
func (s *SocketProcess) drop() {
	if s.conn != nil {
		_ = s.conn.c.Close()
		s.conn = nil
	}
}

// Close implements Executor: it ends the connection, never the plugin.
func (s *SocketProcess) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop()
	return nil
}
