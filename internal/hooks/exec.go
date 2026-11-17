package hooks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxResult bounds what one result may be.
	maxResult = 1 << 20
	// maxStderr is how much of a plugin's stderr a failure keeps.
	maxStderr = 2 << 10
	// waitDelay is how long a killed plugin's pipes are given to close.
	waitDelay = 2 * time.Second
	// Exit codes: 0 ok, 2 permanent, 1 retryable, anything else a
	// crash, also retried.
	exitPermanent = 2
	exitRetryable = 1
)

// ExecProcess starts a fresh process per event: the envelope on stdin,
// the result on stdout, the exit code the verdict.
type ExecProcess struct {
	Command string
	Args    []string
	Timeout time.Duration
	// Launch is the plugin's environment, directory and stderr.
	Launch Launch
}

// Run implements Executor.
func (e *ExecProcess) Run(ctx context.Context, env Envelope) (Result, error) {
	in, err := json.Marshal(env)
	if err != nil {
		return Result{}, Permanent(err)
	}
	cctx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.Command, e.Args...) //nolint:gosec // command and args are the operator-configured plugin, run by design
	if cmd.Env, err = e.Launch.environ(ctx); err != nil {
		return Result{}, &Failure{Class: ClassError, Reason: "start: " + err.Error()}
	}
	if err = e.Launch.prepare(); err != nil {
		return Result{}, &Failure{Class: ClassError, Reason: "start: " + err.Error()}
	}
	cmd.Dir = e.Launch.Dir
	cmd.Stdin = bytes.NewReader(in)
	out, errOut := &capped{max: e.Launch.limit()}, e.Launch.stderr()
	cmd.Stdout, cmd.Stderr = out, errOut
	cmd.WaitDelay = waitDelay
	err = cmd.Run()
	errOut.flush()
	switch {
	case ctx.Err() != nil:
		return Result{}, ctx.Err()
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		return Result{}, &Failure{Class: ClassTimeout, Reason: fmt.Sprintf("timed out after %s", e.Timeout)}
	case err != nil:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return Result{}, &Failure{Permanent: ee.ExitCode() == exitPermanent, Class: exitClass(ee.ExitCode()), Reason: exitReason(ee.ExitCode(), errOut.String())}
		}
		return Result{}, &Failure{Class: ClassCrash, Reason: "start: " + err.Error()}
	case out.over:
		return Result{}, &Failure{Class: ClassMalformed, Reason: fmt.Sprintf("result larger than %d bytes", e.Launch.limit())}
	}
	res, err := ParseResult(out.Bytes())
	if err != nil {
		return Result{}, &Failure{Class: ClassMalformed, Reason: err.Error()}
	}
	return res, nil
}

// Close implements Executor; an exec plugin keeps no process.
func (*ExecProcess) Close() error { return nil }

func exitReason(code int, stderr string) string {
	if stderr == "" {
		return fmt.Sprintf("exit %d", code)
	}
	return fmt.Sprintf("exit %d: %s", code, stderr)
}

// StreamProcess keeps one process and feeds it newline-delimited
// envelopes, reading newline-delimited results that carry the
// envelope's event_id. It is sequential: one envelope is in flight.
type StreamProcess struct {
	Command string
	Args    []string
	Timeout time.Duration
	// Launch is the plugin's environment, directory and stderr.
	Launch Launch

	mu   sync.Mutex
	proc *streamProc
}

type streamProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan []byte
	exited chan struct{} // closed when the process has exited
	code   int           // valid once exited is closed
	stderr *stderrSink
	// overflow is set when a result outgrew the cap.
	overflow atomic.Bool
}

// Run implements Executor.
func (s *StreamProcess) Run(ctx context.Context, env Envelope) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil {
		p, err := s.start()
		if err != nil {
			return Result{}, &Failure{Class: ClassCrash, Reason: "start: " + err.Error()}
		}
		s.proc = p
	}
	p := s.proc
	line, err := json.Marshal(env)
	if err != nil {
		return Result{}, Permanent(err)
	}
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		return Result{}, s.died(p)
	}
	res, closed, err := awaitResult(ctx, env.EventID, p.lines, &p.overflow, s.Timeout, s.Launch.limit())
	switch {
	case closed:
		return Result{}, s.died(p)
	case err != nil:
		s.kill()
		return Result{}, err
	}
	return res, nil
}

// awaitResult reads result lines until the one for eventID, the
// context ends, the timeout passes or the lines end (closed, with no
// error: the caller reports why). Stream and socket plugins share it.
func awaitResult(ctx context.Context, eventID string, lines <-chan []byte, overflow *atomic.Bool, timeout time.Duration, limit int) (res Result, closed bool, err error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return Result{}, false, ctx.Err()
		case <-timer.C:
			return Result{}, false, &Failure{Class: ClassTimeout, Reason: fmt.Sprintf("timed out after %s", timeout)}
		case b, ok := <-lines:
			if !ok {
				if overflow.Load() {
					return Result{}, false, &Failure{Class: ClassMalformed, Reason: fmt.Sprintf("result larger than %d bytes", limit)}
				}
				return Result{}, true, nil
			}
			res, err := ParseResult(b)
			if err != nil {
				return Result{}, false, &Failure{Class: ClassMalformed, Reason: err.Error()}
			}
			if res.EventID != eventID {
				// A late answer to an event given up on: not this one's.
				continue
			}
			return res, false, nil
		}
	}
}

// died reports a process that exited under an envelope, and forgets
// it: the next envelope starts a fresh one.
func (s *StreamProcess) died(p *streamProc) error {
	select {
	case <-p.exited:
	case <-time.After(waitDelay):
		s.kill()
		return &Failure{Class: ClassCrash, Reason: "process closed its output"}
	}
	s.proc = nil
	return &Failure{Permanent: p.code == exitPermanent, Class: exitClass(p.code), Reason: exitReason(p.code, p.stderr.String())}
}

func (s *StreamProcess) kill() {
	if s.proc == nil {
		return
	}
	if s.proc.cmd.Process != nil {
		_ = s.proc.cmd.Process.Kill()
	}
	_ = s.proc.stdin.Close()
	s.proc = nil
}

func (s *StreamProcess) start() (*streamProc, error) {
	env, err := s.Launch.environ(context.Background())
	if err != nil {
		return nil, err
	}
	if err := s.Launch.prepare(); err != nil {
		return nil, err
	}
	cmd := exec.Command(s.Command, s.Args...) //nolint:gosec // command and args are the operator-configured plugin, run by design
	cmd.Env, cmd.Dir = env, s.Launch.Dir
	p := &streamProc{cmd: cmd, lines: make(chan []byte), exited: make(chan struct{}), stderr: s.Launch.stderr()}
	cmd.Stderr = p.stderr
	cmd.WaitDelay = waitDelay
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.stdin = in
	go func() {
		defer close(p.lines)
		r := bufio.NewReaderSize(out, 64<<10)
		for {
			b, err := readLine(r, s.Launch.limit())
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
	go func() {
		_ = cmd.Wait()
		p.stderr.flush()
		p.code = cmd.ProcessState.ExitCode()
		close(p.exited)
	}()
	return p, nil
}

var errLineTooLong = errors.New("line too long")

// readLine reads one line of at most limit bytes.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > limit {
			return nil, errLineTooLong
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return bytes.TrimSpace(line), err
		}
	}
}

// Close implements Executor: it ends the process.
func (s *StreamProcess) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kill()
	return nil
}

// capped keeps up to max bytes and notes the overflow. It wraps its
// buffer rather than embedding it: an embedded bytes.Buffer's ReadFrom
// would be promoted, and io.Copy would use it and bypass the cap.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Bytes() []byte { return c.buf.Bytes() }

func (c *capped) Write(b []byte) (int, error) {
	if room := c.max - c.buf.Len(); len(b) > room {
		c.over = true
		c.buf.Write(b[:max(room, 0)])
		return len(b), nil
	}
	return c.buf.Write(b)
}

// tail keeps the last max bytes written, safe for concurrent use.
type tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(bytes.TrimSpace(t.buf))
}
