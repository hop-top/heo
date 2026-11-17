package hooks

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"

	"hop.top/uma/model"
)

// LineageEntry is one consumer of a plugin: its name, the filter uma
// holds for it (uma fixes a filter when it opens a subscription, so
// reopening the consumer needs the filter it was opened with), and its
// reference once known.
type LineageEntry struct {
	Consumer string                `json:"consumer"`
	Filter   model.Filter          `json:"filter"`
	Ref      model.SubscriptionRef `json:"ref,omitzero"`
}

// Lineage is the consumers a plugin has opened and not yet closed.
// uma lists no subscriptions, so heo keeps the list. Active holds the
// consumers presumed open, the latest last; Retiring those a handover
// has superseded and has yet to close.
type Lineage struct {
	Active   []LineageEntry `json:"active,omitempty"`
	Retiring []LineageEntry `json:"retiring,omitempty"`
}

// LineageStore keeps a plugin's Lineage across restarts. A Store that
// also satisfies it hands a changed filter over without a gap; the
// runner then opens the new consumer at the old one's checkpoint.
type LineageStore interface {
	LoadLineage(ctx context.Context, plugin string) (Lineage, error)
	SaveLineage(ctx context.Context, plugin string, l Lineage) error
}

var _ LineageStore = (*FileStore)(nil)

func (s *FileStore) lineagePath(plugin string) (string, error) {
	p, err := segment(plugin)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir, p, "lineage.json"), nil
}

// LoadLineage implements LineageStore; a plugin with none has the
// zero Lineage.
func (s *FileStore) LoadLineage(_ context.Context, plugin string) (Lineage, error) {
	path, err := s.lineagePath(plugin)
	if err != nil {
		return Lineage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var l Lineage
	if err := read(path, &l); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Lineage{}, err
	}
	return l, nil
}

// SaveLineage implements LineageStore.
func (s *FileStore) SaveLineage(_ context.Context, plugin string, l Lineage) error {
	path, err := s.lineagePath(plugin)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(path, l)
}
