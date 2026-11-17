package hooks

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
)

// The environment of a plugin process. These are the only variables it
// has besides PATH: the daemon's environment, the profile's surface
// token and every credential reference stay behind.
const (
	// EnvName is the plugin's name, as configured.
	EnvName = "HEO_PLUGIN_NAME"
	// EnvToken is the plugin's own bearer token, "<name>.<secret>", the
	// one `heo plugin token mint` printed. It acts as plugin:<name> and
	// as no other actor. Absent when the daemon wires no token source.
	EnvToken = "HEO_PLUGIN_TOKEN" //nolint:gosec // environment variable name, not a credential
	// EnvURL is the base URL of heo's HTTP surface, for read verbs.
	// Absent when no surface address is known.
	EnvURL = "HEO_URL"
)

const (
	// maxStderrLine bounds one logged line of a plugin's stderr; the
	// rest of a longer line is dropped.
	maxStderrLine = 4 << 10
	// dirMode is the permission of a plugin's working directory.
	dirMode = 0o700
)

// Launch is how a plugin process is started, in either mode: who it is,
// where it runs, what it may see and where its stderr goes.
type Launch struct {
	// Name is the plugin's name; it labels the plugin's stderr in the log.
	Name string
	// Dir is the plugin's own working directory. It is created 0700
	// before the process starts. Empty runs in the daemon's directory.
	Dir string
	// URL is the HTTP surface address given to the plugin as EnvURL.
	URL string
	// Token returns the plugin's bearer token, read at each start so a
	// re-minted token is picked up. An error fails the attempt
	// (retryable); an empty token is not set.
	Token func(context.Context) (string, error)
	// Log receives the plugin's stderr, a line at a time, under its
	// name. Nil drops it.
	Log Logger
	// MaxOutput bounds one result on stdout: more is malformed output
	// and fails the attempt. Zero takes maxResult.
	MaxOutput int
}

func (l Launch) limit() int {
	if l.MaxOutput > 0 {
		return l.MaxOutput
	}
	return maxResult
}

// environ is the whole environment of the process.
func (l Launch) environ(ctx context.Context) ([]string, error) {
	env := []string{"PATH=" + os.Getenv("PATH")}
	if l.Name != "" {
		env = append(env, EnvName+"="+l.Name)
	}
	if l.URL != "" {
		env = append(env, EnvURL+"="+l.URL)
	}
	if l.Token != nil {
		tok, err := l.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("plugin token: %w", err)
		}
		if tok != "" {
			env = append(env, EnvToken+"="+tok)
		}
	}
	return env, nil
}

// prepare creates the working directory, owner-only.
func (l Launch) prepare() error {
	if l.Dir == "" {
		return nil
	}
	if err := os.MkdirAll(l.Dir, dirMode); err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone.
	if err := os.Chmod(l.Dir, dirMode); err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	return nil
}

// stderr returns the writer for a process's stderr: lines to the log,
// and the last bytes kept for a failure's reason. flush logs a trailing
// partial line.
func (l Launch) stderr() *stderrSink {
	return &stderrSink{name: l.Name, log: l.Log, keep: &tail{max: maxStderr}}
}

// stderrSink splits a process's stderr into lines for the log, each at
// most maxStderrLine, and keeps its tail.
type stderrSink struct {
	name string
	log  Logger
	keep *tail

	mu      sync.Mutex
	pending []byte
	dropped bool // the current line is past the cap
}

func (s *stderrSink) Write(b []byte) (int, error) {
	n := len(b)
	_, _ = s.keep.Write(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			s.add(b)
			break
		}
		s.add(b[:i])
		s.emit()
		b = b[i+1:]
	}
	return n, nil
}

// add appends to the pending line up to the cap.
func (s *stderrSink) add(b []byte) {
	room := maxStderrLine - len(s.pending)
	if len(b) > room {
		b, s.dropped = b[:max(room, 0)], true
	}
	s.pending = append(s.pending, b...)
}

func (s *stderrSink) emit() {
	line := strings.TrimRight(string(s.pending), "\r")
	if s.dropped {
		line += " [truncated]"
	}
	s.pending, s.dropped = s.pending[:0], false
	if s.log != nil && line != "" {
		s.log.Info("hooks: plugin stderr", "plugin", s.name, "line", line)
	}
}

// flush logs what is left of an unterminated last line.
func (s *stderrSink) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) > 0 {
		s.emit()
	}
}

func (s *stderrSink) String() string { return s.keep.String() }
