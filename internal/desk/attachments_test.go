package desk_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/model"
)

// inject hands the fake channel an inbound message, as its provider
// would, through the channel's webhook.
func (w *world) inject(in adapterstest.Inject) {
	w.t.Helper()
	h, err := w.e.Runtime().Webhook(w.t.Context(),
		uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: service}, Call: call}, w.ch)
	if err != nil {
		w.t.Fatal(err)
	}
	body, err := json.Marshal(in)
	if err != nil {
		w.t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, adapterstest.InjectRoute, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		w.t.Fatalf("inject %s = %d %s", in.Event, rec.Code, rec.Body.String())
	}
}

// received injects a message of ada's, in the local inbox, with the
// attachments, and returns it.
func (w *world) received(event string, atts ...adapterstest.InjectAttachment) model.Message {
	w.t.Helper()
	w.inject(adapterstest.Inject{Event: event, Message: event, From: "ada", Destination: "local", Text: "see attached", Attachments: atts})
	for _, m := range w.messages(w.local.Ref) {
		if len(m.External) > 0 && strings.Contains(m.External[0].ID, event) {
			return m
		}
	}
	w.t.Fatalf("message of %s not found", event)
	return model.Message{}
}

func (w *world) messages(conv model.ConversationRef) []model.Message {
	w.t.Helper()
	p, err := w.client(agent).Messages().List(w.t.Context(), model.ListRequest{Call: call, Filter: model.Filter{Conversation: &conv}, Page: model.PageRequest{Limit: 50}})
	if err != nil {
		w.t.Fatal(err)
	}
	return p.Items
}

// narrow has the provider make the message private.
func (w *world) narrow(event string) {
	w.t.Helper()
	w.inject(adapterstest.Inject{
		Event: event + "-edit", Message: event, Kind: model.EnvelopeMessageEdited, From: "ada", Destination: "local",
		Audience: model.Audience{Visibility: model.Internal}, OccurredAt: time.Now().Add(time.Hour),
	})
}

// redact has the operator redact the message.
func (w *world) redact(m model.Message) {
	w.t.Helper()
	cur, err := w.client(agent).Messages().Get(w.t.Context(), model.Lookup[model.MessageRef]{Call: call, Ref: m.Ref})
	if err != nil {
		w.t.Fatal(err)
	}
	_, err = w.client(operator).Privacy().Redact(w.t.Context(), model.Redact{
		Command: model.Command{Call: call, Operation: "op-redact", ExpectedRevision: cur.Revision},
		Target:  m.Ref.Ref(), Message: &m.Ref, Reason: "test",
	})
	if err != nil {
		w.t.Fatalf("redact: %v", err)
	}
}

func att(key, name, data string) adapterstest.InjectAttachment {
	return adapterstest.InjectAttachment{Key: key, Name: name, MediaType: "text/plain", Data: []byte(data)}
}

func listAtts(t *testing.T, s desk.Service, f desk.Filter, msg string) []desk.Attachment {
	t.Helper()
	p, err := s.ListAttachments(t.Context(), f, msg, model.PageRequest{Limit: 10})
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	return p.Items
}

// TestAttachmentsListGetOpen: the desk lists the attachments of a
// message or a conversation with their metadata and holding messages
// (never bytes), shows one by ref, and opens it to its exact bytes.
func TestAttachmentsListGetOpen(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	m1 := w.received("m1", att("k1", "invoice.txt", "invoice bytes"), att("k2", "photo.txt", "photo"))
	m2 := w.received("m2", att("k1", "invoice.txt", "invoice bytes"))
	w.received("m3")

	byMessage := listAtts(t, s, desk.Filter{}, string(m1.Ref.ID))
	if len(byMessage) != 2 || byMessage[0].Name != "invoice.txt" || byMessage[0].MediaType != "text/plain" ||
		byMessage[0].Size != int64(len("invoice bytes")) || byMessage[0].Audience.Visibility != model.Public ||
		!strings.HasPrefix(byMessage[0].Digest, "sha256:") || byMessage[1].Name != "photo.txt" {
		t.Fatalf("list --message = %+v", byMessage)
	}
	byConv := listAtts(t, s, desk.Filter{Conversation: string(w.local.Ref.ID)}, "")
	if len(byConv) != 2 {
		t.Fatalf("list --conversation = %+v, want the two distinct attachments", byConv)
	}
	var invoice desk.Attachment
	for _, a := range byConv {
		if a.Name == "invoice.txt" {
			invoice = a
		}
	}
	if got := invoice.Messages; len(got) != 2 || got[0] != m1.Ref.ID && got[1] != m1.Ref.ID || got[0] != m2.Ref.ID && got[1] != m2.Ref.ID {
		t.Fatalf("invoice held by %v, want both messages", got)
	}
	if none := listAtts(t, s, desk.Filter{}, string(w.messages(w.local.Ref)[len(w.messages(w.local.Ref))-1].Ref.ID)); len(none) != 0 {
		t.Fatalf("attachments of a message without any = %+v", none)
	}

	got, err := s.GetAttachment(ctx, invoice.Ref)
	if err != nil || got.Name != "invoice.txt" || got.Ref != invoice.Ref || got.Digest != invoice.Digest {
		t.Fatalf("get = %+v, %v", got, err)
	}
	meta, rc, err := s.OpenAttachment(ctx, invoice.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := desk.ReadAll(meta, rc)
	if err != nil || string(b) != "invoice bytes" {
		t.Fatalf("open = %q, %v", b, err)
	}
}

// TestAttachmentRefs: a malformed ref is a usage error; IDs that name
// nothing are not found.
func TestAttachmentRefs(t *testing.T) {
	w := newWorld(t)
	s := w.as(agent)
	for _, bad := range []string{"", "x", "a/", "/b", "a/b/c"} {
		_, err := s.GetAttachment(t.Context(), bad)
		wantCode(t, err, model.Validation, "want <inbox>/<attachment>")
	}
	_, err := s.GetAttachment(t.Context(), "inbox 1/att 1")
	wantCode(t, err, model.NotFound, "not found")
	_, _, err = s.OpenAttachment(t.Context(), string(w.local.Ref.Inbox.ID)+"/0190c1b2-7d3e-7a10-8c11-2f4e5a6b7c8d")
	wantCode(t, err, model.NotFound, "")
}

// TestAttachmentFollowsMessageAudience: once the provider narrows the
// message that holds an attachment, a caller limited to public content
// can no longer list or open it (denied), while the desk still can.
func TestAttachmentFollowsMessageAudience(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	m := w.received("m1", att("k1", "secret.txt", "private bytes"))
	ref := desk.RefName(*m.Content.Parts[1].Attachment)
	for _, who := range []model.ActorID{external, viewer, agent} {
		meta, rc, err := w.as(who).OpenAttachment(ctx, ref)
		if err != nil {
			t.Fatalf("%s before narrowing: %v", who, err)
		}
		if b, _ := desk.ReadAll(meta, rc); string(b) != "private bytes" {
			t.Fatalf("%s read %q", who, b)
		}
		_ = rc.Close()
	}

	w.narrow("m1")

	for _, who := range []model.ActorID{external, viewer} {
		_, _, err := w.as(who).OpenAttachment(ctx, ref)
		wantCode(t, err, model.Unauthorized, "")
		if _, err := w.as(who).GetAttachment(ctx, ref); !model.IsCode(err, model.Unauthorized) {
			t.Fatalf("%s get after narrowing = %v, want unauthorized", who, err)
		}
		if got := listAtts(t, w.as(who), desk.Filter{Conversation: string(w.local.Ref.ID)}, ""); len(got) != 0 {
			t.Fatalf("%s lists %+v after narrowing", who, got)
		}
	}
	// Not "gone": a caller limited to public content cannot tell
	// narrowed from redacted, so it is told only what uma said.
	_, _, err := w.as(external).OpenAttachment(ctx, ref)
	if strings.Contains(err.Error(), "gone") {
		t.Fatalf("external told %q", err)
	}
	meta, rc, err := w.as(agent).OpenAttachment(ctx, ref)
	if err != nil {
		t.Fatalf("desk after narrowing: %v", err)
	}
	_ = rc.Close()
	if meta.Name != "secret.txt" {
		t.Fatalf("meta = %+v", meta)
	}
	if got := listAtts(t, w.as(agent), desk.Filter{Conversation: string(w.local.Ref.ID)}, ""); len(got) != 1 {
		t.Fatalf("desk lists %+v after narrowing", got)
	}
}

// TestRedactedAttachmentIsGone: after the message is redacted nothing
// holds the attachment; the desk, which reads every audience, is told
// it is gone (not found) rather than denied, and it is no longer
// listed.
func TestRedactedAttachmentIsGone(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	m := w.received("m1", att("k1", "pii.txt", "personal data"))
	ref := desk.RefName(*m.Content.Parts[1].Attachment)
	if _, rc, err := w.as(agent).OpenAttachment(ctx, ref); err != nil {
		t.Fatal(err)
	} else {
		_ = rc.Close()
	}

	w.redact(m)

	_, _, err := w.as(agent).OpenAttachment(ctx, ref)
	wantCode(t, err, model.NotFound, "gone")
	_, err = w.as(agent).GetAttachment(ctx, ref)
	wantCode(t, err, model.NotFound, "gone")
	if got := listAtts(t, w.as(agent), desk.Filter{Conversation: string(w.local.Ref.ID)}, ""); len(got) != 0 {
		t.Fatalf("lists %+v after redaction", got)
	}
	_, _, err = w.as(external).OpenAttachment(ctx, ref)
	wantCode(t, err, model.Unauthorized, "")
}

// uma redacts the attachments a redacted message names for good, even
// one another message still holds: the desk is told it is gone, from
// either message, and the other message no longer lists it.
func TestRedactionReachesSharedAttachment(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	m1 := w.received("m1", att("k1", "shared.txt", "shared bytes"))
	m2 := w.received("m2", att("k1", "shared.txt", "shared bytes"))
	ref := desk.RefName(*m1.Content.Parts[1].Attachment)
	w.redact(m1)
	_, _, err := w.as(agent).OpenAttachment(ctx, ref)
	wantCode(t, err, model.NotFound, "gone")
	if got := listAtts(t, w.as(agent), desk.Filter{}, string(m2.Ref.ID)); len(got) != 0 {
		t.Fatalf("m2 lists %+v after the shared attachment was redacted", got)
	}
}

// Whatever uma returned, the messages heo lists attachments of pass the
// caller's audience rule again: with uma allowing the desk everything, a
// caller whose rule does not reach internal content lists no attachment
// of a narrowed message.
func TestListAttachmentsAppliesTheCallersAudienceRule(t *testing.T) {
	w := newWorld(t)
	w.received("m1", att("k1", "secret.txt", "private bytes"))
	w.narrow("m1")
	s := w.as(agent)
	if got := listAtts(t, s, desk.Filter{Conversation: string(w.local.Ref.ID)}, ""); len(got) != 1 {
		t.Fatalf("desk lists %+v", got)
	}
	s.Sees = func(a model.Audience) bool { return a.Visibility == model.Public }
	if got := listAtts(t, s, desk.Filter{Conversation: string(w.local.Ref.ID)}, ""); len(got) != 0 {
		t.Fatalf("a public-only caller lists %+v", got)
	}
}

func TestReadAllRefusesAWrongLength(t *testing.T) {
	a := desk.Attachment{Size: 3}
	if b, err := desk.ReadAll(a, strings.NewReader("abc")); err != nil || string(b) != "abc" {
		t.Fatalf("exact = %q, %v", b, err)
	}
	for _, in := range []string{"ab", "abcd"} {
		if _, err := desk.ReadAll(a, strings.NewReader(in)); err == nil {
			t.Errorf("ReadAll accepted %q for a recorded size of 3", in)
		}
	}
}

// A refusal is not "gone" while a message the caller reads still holds
// the attachment, even for a caller who reads every audience: only the
// absence of every holder says the bytes were removed.
func TestRefusedWhileHeldIsNotGone(t *testing.T) {
	w := newWorld(t)
	m := w.received("m1", att("k1", "held.txt", "held bytes"))
	ref := desk.RefName(*m.Content.Parts[1].Attachment)
	w.gate.denied = map[model.ActorID]bool{agent: true}
	_, _, err := w.as(agent).OpenAttachment(t.Context(), ref)
	wantCode(t, err, model.Unauthorized, "")
}
