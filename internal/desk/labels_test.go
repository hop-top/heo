package desk_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// label creates the label name as the desk agent.
func (w *world) label(name string) model.Label {
	w.t.Helper()
	s := w.as(agent)
	p, err := s.PlanLabelCreate(w.t.Context(), desk.LabelChange{Name: name}, w.cmd().Operation, channels.Submit)
	if err != nil {
		w.t.Fatalf("plan label %s: %v", name, err)
	}
	l, _, err := s.CreateLabel(w.t.Context(), p)
	if err != nil {
		w.t.Fatalf("label %s: %v", name, err)
	}
	return l
}

// labelConv adds (or removes) names on conv as the desk agent.
func (w *world) labelConv(conv model.Conversation, remove bool, names ...string) (desk.LabelingPlan, model.Conversation, model.Acceptance) {
	w.t.Helper()
	s := w.as(agent)
	p, err := s.PlanLabeling(w.t.Context(), string(conv.Ref.ID), names, remove, w.cmd().Operation)
	if err != nil {
		w.t.Fatalf("plan labeling %v: %v", names, err)
	}
	got, acc, err := s.ApplyLabeling(w.t.Context(), p)
	if err != nil {
		w.t.Fatalf("labeling %v: %v", names, err)
	}
	return p, got, acc
}

// labelEvents lists the tenant's label events of kind.
func (w *world) labelEvents(kind string) []model.ConversationLabelPayload {
	w.t.Helper()
	page, err := w.client(agent).Events().List(w.t.Context(), model.ListEvents{
		Call: call, Filter: model.Filter{Kinds: []string{kind}}, Start: model.StartBeginning, Limit: 100,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	var out []model.ConversationLabelPayload
	for _, ev := range page.Items {
		var p model.ConversationLabelPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			w.t.Fatalf("payload %s: %v", ev.Kind, err)
		}
		out = append(out, p)
	}
	return out
}

func labelNames(ls []model.Label) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

func convIDs(cs []model.Conversation) []model.ID {
	out := make([]model.ID, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Ref.ID)
	}
	slices.Sort(out)
	return out
}

// TestLabelCreateAndList: the desk creates tenant labels and lists them;
// a name another label holds under case folding is a conflict in a
// preview, left to uma on submission (which refuses it, explained); a
// retry under the same operation replays; a viewer lists but may not
// create; an invalid name is refused.
func TestLabelCreateAndList(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	p, err := s.PlanLabelCreate(ctx, desk.LabelChange{Name: "billing", Description: "money", Color: "#00aa00"}, "lbl-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	l, acc, err := s.CreateLabel(ctx, p)
	if err != nil || l.Name != "billing" || l.Description != "money" || l.Color != "#00aa00" || l.Ref.ID == "" || acc.Replayed {
		t.Fatalf("create = %+v %+v, %v", l, acc, err)
	}
	again, acc, err := s.CreateLabel(ctx, p)
	if err != nil || !acc.Replayed || again.Ref != l.Ref {
		t.Fatalf("replayed create = %+v %+v, %v", again, acc, err)
	}

	_, err = s.PlanLabelCreate(ctx, desk.LabelChange{Name: "Billing"}, "lbl-2", channels.Preview)
	wantCode(t, err, model.Conflict, `label "Billing"`)
	dup, err := s.PlanLabelCreate(ctx, desk.LabelChange{Name: "Billing"}, "lbl-2", channels.Submit)
	if err != nil {
		t.Fatalf("submit plan of a taken name = %v, want it left to uma", err)
	}
	_, _, err = s.CreateLabel(ctx, dup)
	wantCode(t, err, model.Conflict, `label "Billing"`)
	_, err = s.PlanLabelCreate(ctx, desk.LabelChange{Name: ""}, "lbl-3", channels.Preview)
	wantCode(t, err, model.Validation, "name")
	_, err = s.PlanLabelCreate(ctx, desk.LabelChange{Name: "x", Color: "red"}, "lbl-3", channels.Preview)
	wantCode(t, err, model.Validation, "color")

	w.label("urgent")
	list, err := w.as(viewer).ListLabels(ctx, model.PageRequest{Limit: 10})
	if err != nil || !slices.Equal(labelNames(list.Items), []string{"billing", "urgent"}) {
		t.Fatalf("viewer list = %+v, %v", list.Items, err)
	}
	vp, err := w.as(viewer).PlanLabelCreate(ctx, desk.LabelChange{Name: "spam"}, "lbl-4", channels.Submit)
	if err == nil {
		_, _, err = w.as(viewer).CreateLabel(ctx, vp)
	}
	wantCode(t, err, model.Unauthorized, "")

	got, err := s.FindLabel(ctx, "URGENT", desk.LabelAdd)
	if err != nil || got.Name != "urgent" {
		t.Fatalf("find by folded name = %+v, %v", got, err)
	}
	_, err = s.FindLabel(ctx, "nope", desk.LabelAdd)
	var unknown *desk.UnknownLabelError
	if !errors.As(err, &unknown) || unknown.Name != "nope" || unknown.Use != desk.LabelAdd || !model.IsCode(err, model.NotFound) {
		t.Fatalf("unknown label = %v", err)
	}
}

// TestConversationLabeling: adding a label emits one label-added event
// and moves the revision; adding it again is a no-op success — no event,
// revision unchanged, the plan says so; an unknown label is not found
// and nothing changes; removing a label the conversation does not hold
// is a no-op; removing one it holds emits one label-removed event; a
// name repeated under case folding counts once.
func TestConversationLabeling(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	billing, urgent := w.label("billing"), w.label("urgent")

	p, conv, acc := w.labelConv(w.local, false, "billing", "Billing")
	if !slices.Equal(labelNames(p.Changes), []string{"billing"}) || len(p.Warnings) != 0 ||
		!slices.Equal(conv.Labels, []model.LabelRef{billing.Ref}) || conv.Revision == w.local.Revision || acc.Replayed {
		t.Fatalf("add = plan %+v, conversation %+v", p, conv)
	}
	if evs := w.labelEvents(model.EventConversationLabelAdded); len(evs) != 1 || evs[0].Label != billing.Ref ||
		evs[0].Conversation != w.local.Ref {
		t.Fatalf("label-added events = %+v", evs)
	}

	p, again, _ := w.labelConv(conv, false, "billing")
	if len(p.Changes) != 0 || len(p.Warnings) != 1 || again.Revision != conv.Revision ||
		!slices.Equal(again.Labels, []model.LabelRef{billing.Ref}) {
		t.Fatalf("re-add = plan %+v, conversation %+v", p, again)
	}
	if evs := w.labelEvents(model.EventConversationLabelAdded); len(evs) != 1 {
		t.Fatalf("re-adding emitted %d label-added events", len(evs))
	}

	_, err := w.as(agent).PlanLabeling(ctx, string(conv.Ref.ID), []string{"urgent", "nope"}, false, "lbl-x")
	var unknown *desk.UnknownLabelError
	if !errors.As(err, &unknown) || unknown.Name != "nope" || unknown.Use != desk.LabelAdd {
		t.Fatalf("unknown label = %v", err)
	}
	_, err = w.as(agent).PlanLabeling(ctx, string(conv.Ref.ID), nil, false, "lbl-x")
	wantCode(t, err, model.Validation, "at least one label")
	_, err = w.as(agent).PlanLabeling(ctx, "conv_404", []string{"urgent"}, false, "lbl-x")
	wantCode(t, err, model.NotFound, "conversation conv_404")

	p, same, _ := w.labelConv(conv, true, "urgent")
	if len(p.Changes) != 0 || len(p.Warnings) != 1 || same.Revision != conv.Revision {
		t.Fatalf("remove unheld = plan %+v, conversation %+v", p, same)
	}
	_, _, _ = w.labelConv(conv, false, "urgent")
	p, gone, _ := w.labelConv(conv, true, "billing")
	if !slices.Equal(labelNames(p.Changes), []string{"billing"}) || !slices.Equal(gone.Labels, []model.LabelRef{urgent.Ref}) {
		t.Fatalf("remove = plan %+v, conversation %+v", p, gone)
	}
	if evs := w.labelEvents(model.EventConversationLabelRemoved); len(evs) != 1 || evs[0].Label != billing.Ref {
		t.Fatalf("label-removed events = %+v", evs)
	}
	names, err := w.as(viewer).LabelNames(ctx, gone.Labels)
	if err != nil || !slices.Equal(names, []string{"urgent"}) {
		t.Fatalf("names = %v, %v", names, err)
	}

	// A viewer may not label; uma refuses.
	vp, err := w.as(viewer).PlanLabeling(ctx, string(conv.Ref.ID), []string{"billing"}, false, "lbl-v")
	if err == nil {
		_, _, err = w.as(viewer).ApplyLabeling(ctx, vp)
	}
	wantCode(t, err, model.Unauthorized, "")
}

// TestLabelFilters: --label is any of, --label-all all of, --label-none
// none of, combined by AND, across inboxes, decided by uma; an unknown
// name in a filter is not found.
func TestLabelFilters(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.label("billing")
	w.label("urgent")
	w.label("spam")
	third := w.start(w.local.Ref.Inbox, "cy")
	a, b, c := w.local, w.remote, third
	w.labelConv(a, false, "billing", "urgent")
	w.labelConv(b, false, "billing")
	w.labelConv(c, false, "spam")
	ids := func(cs ...model.Conversation) []model.ID { return convIDs(cs) }

	cases := []struct {
		name string
		f    desk.Filter
		want []model.ID
	}{
		{"any one", desk.Filter{LabelAny: []string{"billing"}}, ids(a, b)},
		{"any two", desk.Filter{LabelAny: []string{"billing", "urgent"}}, ids(a, b)},
		{"any across", desk.Filter{LabelAny: []string{"urgent", "spam"}}, ids(a, c)},
		{"all", desk.Filter{LabelAll: []string{"billing", "urgent"}}, ids(a)},
		{"none", desk.Filter{LabelNone: []string{"billing"}}, ids(c)},
		{"any and none", desk.Filter{LabelAny: []string{"billing"}, LabelNone: []string{"urgent"}}, ids(b)},
		{"all and inbox", desk.Filter{LabelAll: []string{"billing"}, Inbox: "provider"}, ids(b)},
		{"folded name", desk.Filter{LabelAny: []string{"SPAM", "spam"}}, ids(c)},
	}
	for _, tc := range cases {
		got, err := w.as(viewer).ListConversations(ctx, tc.f, model.PageRequest{Limit: 10})
		if err != nil || !slices.Equal(convIDs(got.Items), tc.want) {
			t.Errorf("%s: %v, %v; want %v", tc.name, convIDs(got.Items), err, tc.want)
		}
	}
	_, err := w.as(agent).ListConversations(ctx, desk.Filter{LabelNone: []string{"nope"}}, model.PageRequest{Limit: 10})
	var unknown *desk.UnknownLabelError
	if !errors.As(err, &unknown) || unknown.Name != "nope" || unknown.Use != desk.LabelFilter {
		t.Fatalf("unknown filter label = %v", err)
	}
}
