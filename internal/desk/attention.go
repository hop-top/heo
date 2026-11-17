package desk

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"time"

	"hop.top/uma/model"
)

// Kinds of an attention row.
const (
	// AttentionConversation is a conversation that waits on the caller,
	// with the operations held on it.
	AttentionConversation = "conversation"
	// AttentionOperation is a held operation shown on its own: its target
	// is no conversation (a label create), or a conversation that is not
	// waiting (read, snoozed, resolved).
	AttentionOperation = "operation"
)

// AttentionFilter narrows the attention feed. Inbox is a key or ID.
// UpdatedSince keeps the rows whose time (AttentionItem.At) is after it.
type AttentionFilter struct {
	Inbox        string
	UpdatedSince *time.Time
}

// AttentionItem is one row of the attention feed: a waiting conversation
// with the operations held on it, or a held operation alone. At is when
// the row last changed for the caller: for a conversation, the newest of
// its last activity and the last change of each operation held on it;
// for an operation, its own last change. The feed is newest At first.
type AttentionItem struct {
	Kind string    `json:"kind" yaml:"kind"`
	At   time.Time `json:"at"   yaml:"at"`
	// Inbox is the inbox the row belongs to: empty for a held operation
	// whose target has none (a label).
	Inbox        string            `json:"inbox,omitempty"        yaml:"inbox,omitempty"`
	Conversation *ConversationItem `json:"conversation,omitempty" yaml:"conversation,omitempty"`
	Held         []HeldOperation   `json:"held,omitempty"         yaml:"held,omitempty"`
}

func (i AttentionItem) key() string {
	if i.Conversation != nil {
		return "c:" + string(i.Conversation.Ref.ID)
	}
	return "o:" + i.Held[0].ID
}

const attentionCursorPrefix = "at1."

func attentionCursor(it AttentionItem) model.Cursor {
	raw := it.At.UTC().Format(time.RFC3339Nano) + "|" + it.key()
	return model.Cursor(attentionCursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(raw)))
}

func parseAttentionCursor(c model.Cursor) (time.Time, string, error) {
	bad := model.NewError(model.Validation, fmt.Sprintf("cursor %q: not a cursor of the attention feed; start the listing again", c))
	rest, ok := strings.CutPrefix(string(c), attentionCursorPrefix)
	if !ok {
		return time.Time{}, "", bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return time.Time{}, "", bad
	}
	ts, key, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, "", bad
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", bad
	}
	return t, key, nil
}

// attentionBefore reports whether row a sorts before row b: newer
// first, then the higher key.
func attentionBefore(aAt time.Time, aKey string, bAt time.Time, bKey string) bool {
	if !aAt.Equal(bAt) {
		return aAt.After(bAt)
	}
	return aKey > bKey
}

// ListAttention returns one page of what waits on the caller, newest
// change first: the conversations that are unread for it, open or
// pending and not snoozed, each with the operations held on it, and the
// held operations that belong to no such conversation, each alone. One
// cursor pages the merged feed.
//
// Both sides are uma's reads, as the caller may read them. uma keeps no
// activity time to order by, so heo reads every conversation the filter
// keeps (and, for each, its operations held), derives the row times and
// sorts: the cost is linear in the waiting conversations and the held
// operations, paid before the first page, as it is for `conversation
// list --sort last-activity`. A cursor names the last row of its page and
// is valid with the same filter.
func (s Service) ListAttention(ctx context.Context, f AttentionFilter, page model.PageRequest) (model.Page[AttentionItem], error) {
	var afterAt time.Time
	var afterKey string
	if page.After != "" {
		var err error
		if afterAt, afterKey, err = parseAttentionCursor(page.After); err != nil {
			return model.Page[AttentionItem]{}, err
		}
	}
	cf := Filter{
		Inbox:    f.Inbox,
		Statuses: []string{model.ConversationOpen, model.ConversationPending},
		Workflow: WorkflowFilter{Unread: true, Snoozed: SnoozeExclude},
	}
	claimed := map[string]bool{}
	var rows []AttentionItem
	req := model.PageRequest{Limit: scanPage}
	for {
		p, err := s.ListConversations(ctx, cf, req)
		if err != nil {
			return model.Page[AttentionItem]{}, err
		}
		for _, c := range p.Items {
			it, _, err := s.item(ctx, c, cf, WorkItemFilter{})
			if err != nil {
				return model.Page[AttentionItem]{}, err
			}
			row := AttentionItem{Kind: AttentionConversation, At: it.LastActivity, Inbox: string(c.Ref.Inbox.ID), Conversation: &it}
			ops, err := s.heldOps(ctx, OperationFilter{Inbox: string(c.Ref.Inbox.ID), Conversation: string(c.Ref.ID)})
			if err != nil {
				return model.Page[AttentionItem]{}, err
			}
			for _, op := range ops {
				claimed[string(op.Ref.ID)] = true
				if h, ok := HeldOf(op); ok {
					row.Held = append(row.Held, h)
					if op.UpdatedAt.After(row.At) {
						row.At = op.UpdatedAt
					}
				}
			}
			rows = append(rows, row)
		}
		if p.Complete || p.Next == "" {
			break
		}
		req.After = p.Next
	}
	ops, err := s.heldOps(ctx, OperationFilter{Inbox: f.Inbox})
	if err != nil {
		return model.Page[AttentionItem]{}, err
	}
	for _, op := range ops {
		h, ok := HeldOf(op)
		if !ok || claimed[string(op.Ref.ID)] {
			continue
		}
		var inbox string
		if op.Key != nil {
			inbox = string(op.Key.Target.Inbox)
		}
		rows = append(rows, AttentionItem{Kind: AttentionOperation, At: op.UpdatedAt.UTC(), Inbox: inbox, Held: []HeldOperation{h}})
	}

	kept := rows[:0]
	for _, r := range rows {
		r.At = r.At.UTC()
		if f.UpdatedSince != nil && !r.At.After(*f.UpdatedSince) {
			continue
		}
		if page.After != "" && !attentionBefore(afterAt, afterKey, r.At, r.key()) {
			continue
		}
		kept = append(kept, r)
	}
	slices.SortFunc(kept, func(a, b AttentionItem) int {
		switch {
		case attentionBefore(a.At, a.key(), b.At, b.key()):
			return -1
		case attentionBefore(b.At, b.key(), a.At, a.key()):
			return 1
		}
		return 0
	})
	out := model.Page[AttentionItem]{Items: []AttentionItem{}, Complete: true}
	if len(kept) > page.Limit {
		kept = kept[:page.Limit]
		out.Complete = false
		out.Next = attentionCursor(kept[len(kept)-1])
	}
	out.Items = append(out.Items, kept...)
	return out, nil
}

// heldOps reads every operation uma holds that the caller may read and f
// selects.
func (s Service) heldOps(ctx context.Context, f OperationFilter) ([]model.Operation, error) {
	f.States = []string{string(model.Held)}
	var out []model.Operation
	req := model.PageRequest{Limit: scanPage}
	for {
		p, err := s.ListOperations(ctx, f, req)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if p.Complete || p.Next == "" {
			return out, nil
		}
		req.After = p.Next
	}
}
