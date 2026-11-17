package desk_test

import (
	"slices"
	"testing"
	"time"

	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// activityOrder lists the conversation IDs s lists newest change first.
func activityOrder(t *testing.T, s desk.Service, f desk.Filter) []model.ID {
	t.Helper()
	f.Sort = desk.SortLastActivity
	f.Workflow.Snoozed = desk.SnoozeInclude
	p, err := s.ListConversationItems(t.Context(), f, desk.WorkItemFilter{}, model.PageRequest{Limit: 100})
	if err != nil || !p.Complete {
		t.Fatalf("list %+v = %+v, %v", f, p, err)
	}
	var out []model.ID
	for _, c := range p.Items {
		out = append(out, c.Ref.ID)
	}
	return out
}

// TestLastActivityOrdersAndFilters: the list sorted by last activity puts
// the conversation that changed last first, and reorders when a message
// arrives, the caller's read marker moves or a snooze starts; --updated-since
// keeps only the conversations that changed after it; another actor's
// marker moves nothing for the caller.
func TestLastActivityOrdersAndFilters(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s, s2 := w.as(agent), w.as(agent2)
	local, remote := w.local.Ref.ID, w.remote.Ref.ID

	// The remote conversation started last.
	if got := activityOrder(t, s, desk.Filter{}); !slices.Equal(got, []model.ID{remote, local}) {
		t.Fatalf("initial order = %v, want [%s %s]", got, remote, local)
	}
	// A message arrives in the local conversation.
	w.wake("e-act-1")
	if got := activityOrder(t, s, desk.Filter{}); !slices.Equal(got, []model.ID{local, remote}) {
		t.Errorf("after a message = %v, want local first", got)
	}
	// The caller marks the remote conversation read: its marker moved.
	w.clock.Advance(time.Minute)
	p, err := s.PlanReadMark(ctx, string(remote), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyReadMark(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got := activityOrder(t, s, desk.Filter{}); !slices.Equal(got, []model.ID{remote, local}) {
		t.Errorf("after a read marker moved = %v, want remote first", got)
	}
	// Another actor's marker is its own: nothing moves for the caller.
	w.clock.Advance(time.Minute)
	p2, err := s2.PlanReadMark(ctx, string(local), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.ApplyReadMark(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if got := activityOrder(t, s, desk.Filter{}); !slices.Equal(got, []model.ID{remote, local}) {
		t.Errorf("after another actor's marker = %v, want remote first", got)
	}
	// A snooze starts on the local conversation.
	w.clock.Advance(time.Minute)
	lc := w.refetch(w.local)
	if _, _, err := s.Snooze(ctx, mustPlanSnooze(t, s, string(local), nil, time.Hour, lc.Revision, "op-act-sn")); err != nil {
		t.Fatal(err)
	}
	got := activityOrder(t, s, desk.Filter{})
	if !slices.Equal(got, []model.ID{local, remote}) {
		t.Errorf("after a snooze = %v, want local first", got)
	}

	// --updated-since: after the marker move of remote, only local; after
	// everything, none; the unsorted list honours it too.
	all, err := s.ListConversationItems(ctx, desk.Filter{Sort: desk.SortLastActivity, Workflow: desk.WorkflowFilter{Snoozed: desk.SnoozeInclude}}, desk.WorkItemFilter{}, model.PageRequest{Limit: 100})
	if err != nil || len(all.Items) != 2 {
		t.Fatalf("all = %+v, %v", all, err)
	}
	newest, older := all.Items[0], all.Items[1]
	if !newest.LastActivity.After(older.LastActivity) || older.LastActivity.IsZero() {
		t.Fatalf("activities = %v, %v", newest.LastActivity, older.LastActivity)
	}
	for _, sorted := range []string{"", desk.SortLastActivity} {
		f := desk.Filter{Sort: sorted, UpdatedSince: &older.LastActivity, Workflow: desk.WorkflowFilter{Snoozed: desk.SnoozeInclude}}
		p, err := s.ListConversationItems(ctx, f, desk.WorkItemFilter{}, model.PageRequest{Limit: 100})
		if err != nil || len(p.Items) != 1 || p.Items[0].Ref.ID != newest.Ref.ID {
			t.Errorf("--updated-since the older activity (sort %q) = %+v, %v", sorted, p, err)
		}
		f.UpdatedSince = &newest.LastActivity
		if p, err = s.ListConversationItems(ctx, f, desk.WorkItemFilter{}, model.PageRequest{Limit: 100}); err != nil || len(p.Items) != 0 {
			t.Errorf("--updated-since the newest activity (sort %q) = %+v, %v", sorted, p, err)
		}
	}

	// Pages of a sorted list continue after the last item, and a bad
	// cursor or sort is validation.
	f := desk.Filter{Sort: desk.SortLastActivity, Workflow: desk.WorkflowFilter{Snoozed: desk.SnoozeInclude}}
	p1, err := s.ListConversationItems(ctx, f, desk.WorkItemFilter{}, model.PageRequest{Limit: 1})
	if err != nil || len(p1.Items) != 1 || p1.Complete || p1.Next == "" || p1.Items[0].Ref.ID != newest.Ref.ID {
		t.Fatalf("first page = %+v, %v", p1, err)
	}
	p2b, err := s.ListConversationItems(ctx, f, desk.WorkItemFilter{}, model.PageRequest{Limit: 1, After: p1.Next})
	if err != nil || len(p2b.Items) != 1 || !p2b.Complete || p2b.Items[0].Ref.ID != older.Ref.ID {
		t.Fatalf("second page = %+v, %v", p2b, err)
	}
	if _, err := s.ListConversationItems(ctx, f, desk.WorkItemFilter{}, model.PageRequest{Limit: 1, After: "nope"}); !model.IsCode(err, model.Validation) {
		t.Errorf("bad cursor: %v, want validation", err)
	}
	if _, err := s.ListConversationItems(ctx, desk.Filter{Sort: "name"}, desk.WorkItemFilter{}, model.PageRequest{Limit: 1}); !model.IsCode(err, model.Validation) {
		t.Errorf("bad sort: %v, want validation", err)
	}
}
