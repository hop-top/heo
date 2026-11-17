package xrrx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	xrr "hop.top/xrr"
	xexec "hop.top/xrr/adapters/exec"

	"hop.top/heo/internal/storage/postgres"
)

// redactedStdout is what xrr leaves in place of a stdout it found a
// credential shape in. Replay refuses it: it would hand the code under
// test a placeholder as the tool's output.
const redactedStdout = "<redacted:STDOUT>"

// b64Prefix marks a stdout stored base64: output that is not UTF-8
// (a pg_dump archive) does not survive a YAML string.
const b64Prefix = "base64:"

// Runner returns a postgres.Runner that records and replays inner's
// pg_dump and pg_restore runs through xrr's exec adapter, on the
// process's cassette directory, built on the first run.
//
// A run is keyed by the tool's base name, its arguments with the
// connection string and temp paths scrubbed, and a digest of its stdin:
// never by the OS-chosen parts of it. Its environment, which carries the
// database password, is never recorded.
func (e *Env) Runner(inner postgres.Runner) postgres.Runner {
	return &lazyRunner{env: e, inner: inner}
}

type lazyRunner struct {
	env   *Env
	inner postgres.Runner

	once sync.Once
	sess *xrr.FileSession
	err  error
}

func (l *lazyRunner) Run(ctx context.Context, c postgres.Command) error {
	l.once.Do(func() { l.sess, l.err = l.env.session() })
	if l.err != nil {
		return l.err
	}
	return runRecorded(ctx, l.sess, l.env.MissLog, l.inner, c)
}

// requestOf is the recorded form of c, and its stdin read through.
func requestOf(c postgres.Command) (*xexec.Request, []byte, error) {
	args := make([]string, 0, len(c.Args)+1)
	args = append(args, filepath.Base(c.Path))
	for _, a := range c.Args {
		if i := strings.Index(a, "--dbname="); i == 0 {
			a = "--dbname=<dsn>"
		}
		args = append(args, ScrubRequestText(a))
	}
	req := &xexec.Request{Argv: args}
	var stdin []byte
	if c.Stdin != nil {
		var err error
		if stdin, err = io.ReadAll(c.Stdin); err != nil {
			return nil, nil, fmt.Errorf("read stdin: %w", err)
		}
		sum := sha256.Sum256(stdin)
		req.Stdin = "sha256:" + hex.EncodeToString(sum[:])
	}
	return req, stdin, nil
}

func runRecorded(ctx context.Context, sess *xrr.FileSession, missLog string, inner postgres.Runner, c postgres.Command) error {
	req, stdin, err := requestOf(c)
	if err != nil {
		return err
	}
	adapter := xexec.NewAdapter()
	resp, runErr := sess.Record(ctx, adapter, req, func() (xrr.Response, error) {
		var out bytes.Buffer
		run := c
		if c.Stdin != nil {
			run.Stdin = bytes.NewReader(stdin)
		}
		if c.Stdout != nil {
			run.Stdout = &out
		}
		err := inner.Run(ctx, run)
		return &xexec.Response{Stdout: encodeStdout(out.Bytes()), ExitCode: xexec.ExitCodeFromError(err)}, err
	})
	if errors.Is(runErr, xrr.ErrCassetteMiss) {
		msg := fmt.Sprintf("xrrx: no cassette for %v (stdin %q); re-record with %s=record", req.Argv, req.Stdin, xrr.EnvMode)
		reportMiss(missLog, msg)
		return fmt.Errorf("%s: %w", msg, runErr)
	}
	stdout, ok := stdoutOf(resp)
	if !ok {
		return runErr
	}
	if sess.Mode() == xrr.ModeReplay && stdout == redactedStdout {
		return errors.New("xrrx: the recorded stdout was redacted by xrr; re-record a run whose output carries no credential")
	}
	if c.Stdout != nil {
		b, derr := decodeStdout(stdout)
		if derr != nil {
			return derr
		}
		if _, werr := c.Stdout.Write(b); werr != nil {
			return werr
		}
	}
	return runErr
}

// stdoutOf reads stdout out of a recorded (typed) or replayed (raw)
// response.
func stdoutOf(resp xrr.Response) (string, bool) {
	switch r := resp.(type) {
	case *xexec.Response:
		return r.Stdout, true
	case *xrr.RawResponse:
		s, _ := r.Payload["stdout"].(string)
		return s, true
	}
	return "", false
}

func encodeStdout(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return b64Prefix + base64.StdEncoding.EncodeToString(b)
}

func decodeStdout(s string) ([]byte, error) {
	rest, ok := strings.CutPrefix(s, b64Prefix)
	if !ok {
		return []byte(s), nil
	}
	b, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		return nil, fmt.Errorf("xrrx: recorded stdout: %w", err)
	}
	return b, nil
}
