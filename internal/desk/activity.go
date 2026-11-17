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

// SortLastActivity orders a conversation listing by last activity, newest
// first.
const SortLastActivity = "last-activity"

// Sorts lists the values of `--sort`.
func Sorts() []string { return []string{SortLastActivity} }

// LastActivity is when c last changed for the caller: the newest of the
// recording time of its newest message the caller may read (imported
// history is old news and does not count), the time its snooze was set,
// and the time the caller last moved its own read marker. The snooze and
// the marker count only for a caller that may read them, so an external
// actor's activity is its messages alone. It is the zero time for a
// conversation with none of them. heo derives it from what uma exposes;
// it stores nothing.
func (s Service) LastActivity(ctx context.Context, c model.Conversation) (time.Time, error) {
	var at time.Time
	later := func(t time.Time) {
		if t.After(at) {
			at = t
		}
	}
	req := model.ListRequest{Call: call(), Filter: model.Filter{Conversation: &c.Ref}, Page: model.PageRequest{Limit: scanPage}}
	for {
		p, err := s.Client.Messages().List(ctx, req)
		if err != nil {
			return time.Time{}, err
		}
		for _, m := range s.visible(p.Items, false) {
			if m.ImportedBy == "" {
				later(m.RecordedAt)
			}
		}
		if p.Complete || p.Next == "" {
			break
		}
		req.Page.After = p.Next
	}
	if c.Snooze != nil {
		later(c.Snooze.At)
	}
	rs, err := s.ReadState(ctx, c)
	if err != nil {
		return time.Time{}, err
	}
	if rs != nil && rs.Marked {
		later(rs.UpdatedAt)
	}
	return at.UTC(), nil
}

// activityCursorPrefix starts the cursor of a listing by last activity.
const activityCursorPrefix = "la1."

// activityCursor is the position after an item of a listing by last
// activity: the item's activity and ID. uma's own cursor cannot carry it,
// as the order is heo's.
func activityCursor(it ConversationItem) model.Cursor {
	raw := it.LastActivity.UTC().Format(time.RFC3339Nano) + "|" + string(it.Ref.ID)
	return model.Cursor(activityCursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(raw)))
}

func parseActivityCursor(c model.Cursor) (time.Time, model.ID, error) {
	bad := model.NewError(model.Validation, fmt.Sprintf("cursor %q: not a cursor of a listing by %s; start the listing again", c, SortLastActivity))
	rest, ok := strings.CutPrefix(string(c), activityCursorPrefix)
	if !ok {
		return time.Time{}, "", bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return time.Time{}, "", bad
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, "", bad
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", bad
	}
	return t, model.ID(id), nil
}

// before reports whether a sorts before b: newer activity first, then
// the higher ID.
func before(aAt time.Time, aID model.ID, bAt time.Time, bID model.ID) bool {
	if !aAt.Equal(bAt) {
		return aAt.After(bAt)
	}
	return aID > bID
}

// listByActivity is ListConversationItems ordered by last activity,
// newest first. uma lists in its own order and keeps no activity time to
// order by, so heo reads every conversation the filters keep, derives
// each one's activity and sorts; the page is cut after the cursor, which
// names the last item of the previous page, so a conversation that
// changed between pages moves without repeating or hiding the others.
func (s Service) listByActivity(ctx context.Context, f Filter, wi WorkItemFilter, page model.PageRequest) (model.Page[ConversationItem], error) {
	var afterAt time.Time
	var afterID model.ID
	if page.After != "" {
		var err error
		if afterAt, afterID, err = parseActivityCursor(page.After); err != nil {
			return model.Page[ConversationItem]{}, err
		}
	}
	var all []ConversationItem
	req := model.PageRequest{Limit: scanPage}
	for {
		p, err := s.ListConversations(ctx, f, req)
		if err != nil {
			return model.Page[ConversationItem]{}, err
		}
		for _, c := range p.Items {
			it, ok, err := s.item(ctx, c, f, wi)
			if err != nil {
				return model.Page[ConversationItem]{}, err
			}
			if ok && (page.After == "" || before(afterAt, afterID, it.LastActivity, it.Ref.ID)) {
				all = append(all, it)
			}
		}
		if p.Complete || p.Next == "" {
			break
		}
		req.After = p.Next
	}
	slices.SortFunc(all, func(a, b ConversationItem) int {
		switch {
		case before(a.LastActivity, a.Ref.ID, b.LastActivity, b.Ref.ID):
			return -1
		case before(b.LastActivity, b.Ref.ID, a.LastActivity, a.Ref.ID):
			return 1
		}
		return 0
	})
	out := model.Page[ConversationItem]{Items: []ConversationItem{}, Complete: true}
	if len(all) > page.Limit {
		all = all[:page.Limit]
		out.Complete = false
		out.Next = activityCursor(all[len(all)-1])
	}
	out.Items = append(out.Items, all...)
	return out, nil
}
