package approvechat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Announced is one announcement the handler posted: the message of an
// approval conversation that announces Operation.
type Announced struct {
	Conversation string `json:"conversation"` // Conversation.Key()
	Message      string `json:"message"`
	Operation    string `json:"operation"`
	// Closed is the outcome (approved, rejected, expired) of an
	// announcement whose operation closed, and ClosedAt when; both are
	// absent while it is open, as in files written before they existed.
	Closed   string     `json:"closed,omitempty"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
}

// State is what the handler keeps across a restart: the announcements
// still open (so a restarted daemon edits them when their operation is
// decided or expires, and does not announce them again) and the replies
// and reactions already handled (so a redelivered one decides nothing
// twice).
type State struct {
	Announcements []Announced `json:"announcements,omitempty"`
	Handled       []string    `json:"handled,omitempty"`
}

// Store keeps State. uma holds no record of a consumer's own, so heo
// keeps this beside the daemon's other local state, as it does the
// hooks' dead letters.
type Store interface {
	// Load returns the saved state; none saved is the zero State.
	Load() (State, error)
	// Save replaces the saved state.
	Save(State) error
}

// FileStore is a Store of one owner-only JSON file, replaced atomically
// (temporary file, then rename).
type FileStore struct{ path string }

// NewFileStore returns a FileStore of the file dir/approval-chat.json;
// dir is created on the first save.
func NewFileStore(dir string) *FileStore {
	return &FileStore{path: filepath.Join(dir, "approval-chat.json")}
}

var _ Store = (*FileStore)(nil)

// Load reads the file.
func (s *FileStore) Load() (State, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("%s: %w", s.path, err)
	}
	return st, nil
}

// Save writes the file.
func (s *FileStore) Save(st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(b, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o600)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), s.path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// WithStore has h keep its state in st and restores what st holds: the
// announcements of conversations still configured, and the handled
// events. A load that fails is logged and leaves h starting empty; a
// save that fails is logged and leaves h running on memory. It returns h.
func (h *Handler) WithStore(st Store) *Handler {
	h.persistMu.Lock()
	defer h.persistMu.Unlock()
	h.store = st
	if st == nil {
		return h
	}
	state, err := st.Load()
	if err != nil {
		h.log.Error("approval chat: saved state not read; starting empty", "error", err)
		return h
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range state.Announcements {
		c, ok := h.convByKey(a.Conversation)
		if !ok {
			continue // no longer an approval conversation
		}
		if a.Closed != "" {
			if a.ClosedAt != nil {
				h.closed[a.Conversation+"\x00"+a.Message] = closedRef{conv: c, message: a.Message, op: a.Operation, outcome: a.Closed, at: *a.ClosedAt}
			}
			continue
		}
		h.anns[a.Conversation+"\x00"+a.Message] = a.Operation
		h.byOp[a.Operation] = append(h.byOp[a.Operation], annRef{c, a.Message})
	}
	h.pruneClosed()
	for _, e := range state.Handled {
		if !h.seen[e] {
			h.seen[e] = true
			h.ring = append(h.ring, e)
		}
	}
	return h
}

func (h *Handler) convByKey(key string) (Conversation, bool) {
	for _, c := range h.cfg.Conversations {
		if c.Key() == key {
			return c, true
		}
	}
	return Conversation{}, false
}

// persist saves the current state.
func (h *Handler) persist() {
	h.persistMu.Lock()
	defer h.persistMu.Unlock()
	if h.store == nil {
		return
	}
	h.mu.Lock()
	var st State
	for op, refs := range h.byOp {
		for _, a := range refs {
			st.Announcements = append(st.Announcements, Announced{Conversation: a.conv.Key(), Message: a.message, Operation: op})
		}
	}
	h.pruneClosed()
	for _, c := range h.closed {
		at := c.at
		st.Announcements = append(st.Announcements, Announced{Conversation: c.conv.Key(), Message: c.message, Operation: c.op, Closed: c.outcome, ClosedAt: &at})
	}
	st.Handled = append(st.Handled, h.ring...)
	h.mu.Unlock()
	sort.Slice(st.Announcements, func(i, j int) bool {
		a, b := st.Announcements[i], st.Announcements[j]
		return a.Conversation+a.Message < b.Conversation+b.Message
	})
	if err := h.store.Save(st); err != nil {
		h.log.Error("approval chat: state not saved; a restart forgets it", "error", err)
	}
}
