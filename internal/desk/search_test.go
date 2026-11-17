package desk_test

import (
	"slices"
	"testing"
	"time"

	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// textOf is the text of the messages of ms, sorted.
func textOf(ms []model.Message) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, desk.Text(m.Content))
	}
	slices.Sort(out)
	return out
}

// TestMessageSearchByText: the desk finds the messages holding every
// word of the text, across inboxes, uma matching them; a word no message
// holds finds none; the conversation and inbox filters combine with it.
func TestMessageSearchByText(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	page := model.PageRequest{Limit: 10}

	got, err := s.ListMessages(ctx, desk.Filter{Text: "hello"}, page)
	if err != nil || !slices.Equal(textOf(got.Items), []string{"hello ada", "hello bob"}) {
		t.Fatalf("search hello = %v, %v", textOf(got.Items), err)
	}
	got, err = s.ListMessages(ctx, desk.Filter{Text: "BOB hello"}, page)
	if err != nil || !slices.Equal(textOf(got.Items), []string{"hello bob"}) {
		t.Fatalf("search BOB hello = %v, %v", textOf(got.Items), err)
	}
	got, err = s.ListMessages(ctx, desk.Filter{Text: "invoice"}, page)
	if err != nil || len(got.Items) != 0 {
		t.Fatalf("search invoice = %v, %v", textOf(got.Items), err)
	}
	got, err = s.ListMessages(ctx, desk.Filter{Text: "hello", Inbox: "local"}, page)
	if err != nil || !slices.Equal(textOf(got.Items), []string{"hello ada"}) {
		t.Fatalf("search hello in local = %v, %v", textOf(got.Items), err)
	}
	got, err = s.ListMessages(ctx, desk.Filter{Text: "hello", Conversation: string(w.remote.Ref.ID)}, page)
	if err != nil || !slices.Equal(textOf(got.Items), []string{"hello bob"}) {
		t.Fatalf("search hello in remote conversation = %v, %v", textOf(got.Items), err)
	}
	// uma's limits are uma's: text of no word is refused by uma.
	_, err = s.ListMessages(ctx, desk.Filter{Text: "!!!"}, page)
	wantCode(t, err, model.Validation, "")
}

// TestMessageSearchNeverReachesNotesOfOthers: an internal note matches
// its words for the desk and for nobody else: a viewer's search for the
// note's words returns nothing, whichever words and filters it tries.
func TestMessageSearchNeverReachesNotesOfOthers(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	note := w.note("refund approved by finance")
	page := model.PageRequest{Limit: 10}

	got, err := w.as(agent).ListMessages(ctx, desk.Filter{Text: "refund finance"}, page)
	if err != nil || len(got.Items) != 1 || got.Items[0].Ref != note {
		t.Fatalf("desk search = %+v, %v; want the note", got.Items, err)
	}
	for _, f := range []desk.Filter{
		{Text: "refund"},
		{Text: "refund finance", Inbox: "local"},
		{Text: "approved", Conversation: string(w.local.Ref.ID)},
	} {
		got, err = w.as(viewer).ListMessages(ctx, f, page)
		if err != nil || len(got.Items) != 0 {
			t.Fatalf("viewer search %+v = %+v, %v; want none", f, got.Items, err)
		}
	}
	// What the viewer may read stays searchable.
	got, err = w.as(viewer).ListMessages(ctx, desk.Filter{Text: "ada"}, page)
	if err != nil || !slices.Equal(textOf(got.Items), []string{"hello ada"}) {
		t.Fatalf("viewer search ada = %v, %v", textOf(got.Items), err)
	}
}

// TestMessageSearchByTime: the half-open range [since, until) bounds the
// message's own time, uma comparing; an empty range is uma's refusal.
func TestMessageSearchByTime(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	page := model.PageRequest{Limit: 10}
	m := w.local
	all, err := s.ListMessages(ctx, desk.Filter{Conversation: string(m.Ref.ID)}, page)
	if err != nil || len(all.Items) != 1 {
		t.Fatalf("messages = %+v, %v", all.Items, err)
	}
	at := all.Items[0].OccurredAt
	before, after := at.Add(-time.Hour), at.Add(time.Hour)

	for _, tc := range []struct {
		name         string
		since, until *time.Time
		want         int
	}{
		{"since before", &before, nil, 1},
		{"since at", &at, nil, 1},
		{"since after", &after, nil, 0},
		{"until at", nil, &at, 0},
		{"until after", nil, &after, 1},
		{"window", &before, &after, 1},
	} {
		got, err := s.ListMessages(ctx, desk.Filter{Conversation: string(m.Ref.ID), Since: tc.since, Until: tc.until}, page)
		if err != nil || len(got.Items) != tc.want {
			t.Errorf("%s = %+v, %v; want %d", tc.name, got.Items, err, tc.want)
		}
	}
	_, err = s.ListMessages(ctx, desk.Filter{Since: &after, Until: &before}, page)
	wantCode(t, err, model.Validation, "")
}

// TestMessageSearchByLabels: label filters pass to uma beside the text
// and select messages by the labels their conversation holds when read:
// any, all and none; a label removed stops matching; an unknown label is
// not found.
func TestMessageSearchByLabels(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.label("billing")
	w.label("urgent")
	w.labelConv(w.local, false, "billing", "urgent")
	w.labelConv(w.remote, false, "billing")
	s := w.as(agent)
	page := model.PageRequest{Limit: 10}
	want := func(name string, f desk.Filter, texts ...string) {
		t.Helper()
		got, err := s.ListMessages(ctx, f, page)
		if err != nil || !slices.Equal(textOf(got.Items), texts) {
			t.Errorf("%s = %v, %v; want %v", name, textOf(got.Items), err, texts)
		}
	}
	want("any billing", desk.Filter{Text: "hello", LabelAny: []string{"billing"}}, "hello ada", "hello bob")
	want("all billing urgent", desk.Filter{Text: "hello", LabelAll: []string{"billing", "urgent"}}, "hello ada")
	want("none urgent", desk.Filter{Text: "hello", LabelNone: []string{"urgent"}}, "hello bob")
	want("labels without text", desk.Filter{LabelAll: []string{"urgent"}}, "hello ada")
	want("folded name", desk.Filter{Text: "hello", LabelAny: []string{"URGENT"}}, "hello ada")
	want("text misses", desk.Filter{Text: "bob", LabelAll: []string{"urgent"}})

	w.labelConv(w.local, true, "urgent")
	want("removed label", desk.Filter{Text: "hello", LabelAny: []string{"urgent"}})

	_, err := s.ListMessages(ctx, desk.Filter{Text: "hello", LabelAny: []string{"nope"}}, page)
	wantCode(t, err, model.NotFound, `label "nope"`)
}
