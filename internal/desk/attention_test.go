package desk_test

import (
	"slices"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// attentionIDs names the rows of an attention page, in order: the
// conversation ID of a conversation row, the operation ID of an
// operation row.
func attentionIDs(items []desk.AttentionItem) []string {
	var out []string
	for _, it := range items {
		switch it.Kind {
		case desk.AttentionConversation:
			out = append(out, "c:"+string(it.Conversation.Ref.ID))
		default:
			out = append(out, "o:"+it.Held[0].ID)
		}
	}
	return out
}

// holdLabelCreate has the plugin actor propose a label create: held, with
// no conversation.
func holdLabelCreate(t *testing.T, w *world, key string) model.OperationRef {
	t.Helper()
	s := w.as(plugin)
	p, err := s.PlanLabelCreate(t.Context(), desk.LabelChange{Name: "bill-" + key}, model.OperationID(key), channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	p.Request.Hold = &model.HoldProposal{
		Reason: "new label", TrustKey: "triage/label-create",
		Preview: &model.Extension{Name: desk.PreviewExtension, Version: 1, Data: []byte(`{"would":"create"}`)},
	}
	_, acc, err := s.CreateLabel(t.Context(), p)
	if err != nil || acc.Operation.State != model.Held {
		t.Fatalf("hold label: %v, %v", acc.Operation.State, err)
	}
	return acc.Operation.Ref
}

// TestAttentionMergesWaitingAndHeld: two inboxes; the local conversation
// is unread and holds a proposed message edit, the remote one is read
// (the actor marked it) and holds nothing; a proposed label create has no
// conversation. One feed lists the waiting conversation with its held
// operation and the label create as its own row, newest change first; one
// cursor pages it; the inbox filter narrows it; a read, snoozed or
// resolved conversation is left out, a held operation never is.
func TestAttentionMergesWaitingAndHeld(t *testing.T) {
	w := optInWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	local, remote := w.local.Ref.ID, w.remote.Ref.ID

	// A message arrives in each conversation, the remote one last: both
	// are unread for the actor until it marks one.
	w.wake("att-in-local")
	w.clock.Advance(time.Minute)
	w.inject(adapterstest.Inject{Event: "att-in-remote", Message: "att-in-remote", From: "bob", Destination: "provider", Text: "hello?"})
	all := func(f desk.AttentionFilter, page model.PageRequest) model.Page[desk.AttentionItem] {
		t.Helper()
		p, err := s.ListAttention(ctx, f, page)
		if err != nil {
			t.Fatalf("attention %+v: %v", f, err)
		}
		return p
	}
	if got := attentionIDs(all(desk.AttentionFilter{}, model.PageRequest{Limit: 10}).Items); !slices.Equal(got, []string{"c:" + string(remote), "c:" + string(local)}) {
		t.Fatalf("two unread = %v", got)
	}

	// The actor reads the remote conversation: it is no longer waiting.
	w.clock.Advance(time.Minute)
	pl, err := s.PlanReadMark(ctx, string(remote), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyReadMark(ctx, pl); err != nil {
		t.Fatal(err)
	}
	// A message edit is held on the local conversation, then a label create.
	w.clock.Advance(time.Minute)
	edit := holdEdit(t, w, "att-edit")
	w.clock.Advance(time.Minute)
	label := holdLabelCreate(t, w, "att-label")

	p := all(desk.AttentionFilter{}, model.PageRequest{Limit: 10})
	if got, want := attentionIDs(p.Items), []string{"o:" + string(label.ID), "c:" + string(local)}; !slices.Equal(got, want) {
		t.Fatalf("feed = %v, want %v", got, want)
	}
	row := p.Items[1]
	if len(row.Held) != 1 || row.Held[0].ID != string(edit.ID) || row.Inbox != string(w.local.Ref.Inbox.ID) || row.Conversation == nil {
		t.Fatalf("conversation row = %+v", row)
	}
	if lab := p.Items[0]; lab.Conversation != nil || lab.Inbox != "" || len(lab.Held) != 1 || lab.Held[0].Kind != model.ActionLabelConfigure {
		t.Fatalf("label row = %+v", lab)
	}
	// The row's time is its newest change: the held edit, after the last message.
	if !row.At.After(row.Conversation.LastActivity) {
		t.Errorf("row at %v not after conversation activity %v", row.At, row.Conversation.LastActivity)
	}

	// A new message moves the conversation row above the label.
	w.clock.Advance(time.Minute)
	w.wake("att-wake")
	if got := attentionIDs(all(desk.AttentionFilter{}, model.PageRequest{Limit: 10}).Items); !slices.Equal(got, []string{"c:" + string(local), "o:" + string(label.ID)}) {
		t.Errorf("after a message = %v", got)
	}

	// One cursor pages the merged feed.
	p1 := all(desk.AttentionFilter{}, model.PageRequest{Limit: 1})
	if got := attentionIDs(p1.Items); p1.Complete || p1.Next == "" || !slices.Equal(got, []string{"c:" + string(local)}) {
		t.Fatalf("first page = %v %+v", got, p1)
	}
	p2 := all(desk.AttentionFilter{}, model.PageRequest{Limit: 1, After: p1.Next})
	if got := attentionIDs(p2.Items); !p2.Complete || !slices.Equal(got, []string{"o:" + string(label.ID)}) {
		t.Fatalf("second page = %v %+v", got, p2)
	}
	if _, err := s.ListAttention(ctx, desk.AttentionFilter{}, model.PageRequest{Limit: 1, After: "la1.x"}); !model.IsCode(err, model.Validation) {
		t.Errorf("a conversation cursor: %v, want validation", err)
	}

	// The inbox filter: the local inbox has the conversation and its edit;
	// the remote one has nothing; the tenant-wide label has no inbox.
	if got := attentionIDs(all(desk.AttentionFilter{Inbox: "local"}, model.PageRequest{Limit: 10}).Items); !slices.Equal(got, []string{"c:" + string(local)}) {
		t.Errorf("inbox local = %v", got)
	}
	if got := all(desk.AttentionFilter{Inbox: "provider"}, model.PageRequest{Limit: 10}).Items; len(got) != 0 {
		t.Errorf("inbox provider = %v, want none", attentionIDs(got))
	}

	// Since: only what changed after the point.
	since := p.Items[0].At
	if got := attentionIDs(all(desk.AttentionFilter{UpdatedSince: &since}, model.PageRequest{Limit: 10}).Items); !slices.Equal(got, []string{"c:" + string(local)}) {
		t.Errorf("updated since = %v", got)
	}

	// The actor reads the local conversation: its held edit stays, as a row of its own.
	w.clock.Advance(time.Minute)
	pl, err = s.PlanReadMark(ctx, string(local), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyReadMark(ctx, pl); err != nil {
		t.Fatal(err)
	}
	got := attentionIDs(all(desk.AttentionFilter{}, model.PageRequest{Limit: 10}).Items)
	if !slices.Equal(got, []string{"o:" + string(label.ID), "o:" + string(edit.ID)}) && !slices.Equal(got, []string{"o:" + string(edit.ID), "o:" + string(label.ID)}) {
		t.Errorf("after reading = %v, want the two held operations", got)
	}
}
