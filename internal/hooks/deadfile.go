package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	storeDirMode  = 0o700
	storeFileMode = 0o600
)

// FileStore is a Store of one JSON file per record, owner-only:
//
//	<dir>/<plugin>/dead/<event>.json      a dead letter
//	<dir>/<plugin>/attempts/<event>.json  an event in retry
//
// uma's store holds delivery dead letters as operation state, and has no
// place for records of a consumer's own; heo owns no tables, so these
// live beside the daemon's other local state. Writes are atomic
// (temporary file, then rename).
type FileStore struct {
	dir string
	now func() time.Time

	mu sync.Mutex
}

var _ Store = (*FileStore)(nil)

// NewFileStore returns a FileStore under dir; it is created on first
// write.
func NewFileStore(dir string) *FileStore { return &FileStore{dir: dir, now: time.Now} }

func segment(s string) (string, error) {
	if s == "" || s == "." || s == ".." {
		return "", fmt.Errorf("invalid name %q", s)
	}
	return url.PathEscape(s), nil
}

func (s *FileStore) path(plugin, kind, event string) (string, error) {
	p, err := segment(plugin)
	if err != nil {
		return "", err
	}
	e, err := segment(event)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir, p, kind, strings.ReplaceAll(e, "%", "%25")+".json"), nil
}

func (s *FileStore) write(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), storeDirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(b, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), storeFileMode)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

func read(path string, v any) error {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	return dec.Decode(v)
}

// Record implements DeadLetterer.
func (s *FileStore) Record(_ context.Context, d DeadLetter) error {
	path, err := s.path(d.Plugin, "dead", string(d.Event.ID))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := newRecord(d)
	var prev DeadRecord
	switch err := read(path, &prev); {
	case err == nil:
		rec = rec.merge(prev, d.Replay)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return s.write(path, rec)
}

// Get implements Store.
func (s *FileStore) Get(_ context.Context, plugin, event string) (DeadRecord, error) {
	path, err := s.path(plugin, "dead", event)
	if err != nil {
		return DeadRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return getRecord(path)
}

func getRecord(path string) (DeadRecord, error) {
	var r DeadRecord
	switch err := read(path, &r); {
	case errors.Is(err, fs.ErrNotExist):
		return DeadRecord{}, ErrNoDeadLetter
	case err != nil:
		return DeadRecord{}, fmt.Errorf("read dead letter: %w", err)
	}
	return r, nil
}

// List implements Store.
func (s *FileStore) List(_ context.Context, plugin string, state DeadState) ([]DeadRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var dirs []string
	if plugin != "" {
		p, err := segment(plugin)
		if err != nil {
			return nil, err
		}
		dirs = []string{filepath.Join(s.dir, p, "dead")}
	} else {
		ents, err := os.ReadDir(s.dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, e := range ents {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(s.dir, e.Name(), "dead"))
			}
		}
	}
	var out []DeadRecord
	for _, dir := range dirs {
		ents, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			r, err := getRecord(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			if state == "" || r.State == state {
				out = append(out, r)
			}
		}
	}
	slices.SortFunc(out, func(a, b DeadRecord) int {
		switch {
		case a.FirstAt.Before(b.FirstAt):
			return -1
		case b.FirstAt.Before(a.FirstAt):
			return 1
		}
		return strings.Compare(a.Plugin+"/"+a.Event, b.Plugin+"/"+b.Event)
	})
	return out, nil
}

// Resolve implements Store.
func (s *FileStore) Resolve(_ context.Context, plugin, event string, state DeadState, at time.Time) (DeadRecord, error) {
	if state != StateReplayed && state != StateDiscarded {
		return DeadRecord{}, fmt.Errorf("cannot resolve to %q", state)
	}
	path, err := s.path(plugin, "dead", event)
	if err != nil {
		return DeadRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := getRecord(path)
	if err != nil {
		return DeadRecord{}, err
	}
	if r.State != StateDead {
		return r, notDead(r)
	}
	r.State, r.ResolvedAt = state, at.UTC()
	return r, s.write(path, r)
}

// Load implements Attempts.
func (s *FileStore) Load(_ context.Context, plugin, event string) (AttemptState, error) {
	path, err := s.path(plugin, "attempts", event)
	if err != nil {
		return AttemptState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var a AttemptState
	if err := read(path, &a); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return AttemptState{}, err
	}
	return a, nil
}

// Save implements Attempts.
func (s *FileStore) Save(_ context.Context, plugin, event string, a AttemptState) error {
	path, err := s.path(plugin, "attempts", event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(path, a)
}

// Clear implements Attempts.
func (s *FileStore) Clear(_ context.Context, plugin, event string) error {
	path, err := s.path(plugin, "attempts", event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
