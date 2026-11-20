package cmd

import (
	"os"
	"strings"
	"testing"

	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// TestMessageForwardPlan: the preview names the recipient, says the
// original travels attached whole, shows its size and says how the copy is
// found; it says the mail cannot be recalled.
func TestMessageForwardPlan(t *testing.T) {
	p := forwardPlan(desk.ForwardPlan{
		Request:      model.ExecuteExtension{Audience: model.Audience{Visibility: model.Internal}, Action: model.Extension{Name: "google.mail.forward", Version: 1, Data: []byte(`{"to":"d@example.test"}`)}},
		Conversation: testTagConversation(), Forward: desk.MailForward{To: "d@example.test"},
		Message: "msg_1", Provider: "18c0ffee", OriginalID: "<o@x.y>", Size: 2048,
	}, "key-1")
	if len(p.Effects) != 1 || p.Effects[0].Reversible || p.Effects[0].Target != "mail:message 18c0ffee" {
		t.Fatalf("effects = %+v", p.Effects)
	}
	for _, want := range []string{"d@example.test", "message/rfc822", "irreversible", "third party", "2048 bytes"} {
		if !strings.Contains(p.Effects[0].Detail, want) {
			t.Errorf("effect detail lacks %q: %s", want, p.Effects[0].Detail)
		}
	}
	for k, want := range map[string]string{"to": "d@example.test", "original_message_id": "<o@x.y>", "operation_key": "key-1"} {
		if p.Args[k] != want {
			t.Errorf("arg %s = %v, want %s", k, p.Args[k], want)
		}
	}
	for _, k := range []string{"size", "carries", "finds", "attachment"} {
		if s, _ := p.Args[k].(string); s == "" {
			t.Errorf("arg %s missing", k)
		}
	}
	unknown := forwardPlan(desk.ForwardPlan{Forward: desk.MailForward{To: "d@example.test"}, Conversation: testTagConversation(), Size: -1}, "")
	if !strings.Contains(unknown.Effects[0].Detail, "size unknown") {
		t.Errorf("unknown size detail = %s", unknown.Effects[0].Detail)
	}
}

// TestMessageForwardFlagsAndHold: --to is required, the list filters
// exist, and the forward goes through deskHeld and the hold point.
func TestMessageForwardFlagsAndHold(t *testing.T) {
	a := newApp()
	fw := find(a.r.Cmd, "message forward")
	if fw == nil || fw.Flags().Lookup("to") == nil || fw.Flags().Lookup("text") == nil {
		t.Fatal("message forward flags missing")
	}
	ls := find(a.r.Cmd, "message list")
	for _, f := range []string{"message-id", "references", "provider-id"} {
		if ls.Flags().Lookup(f) == nil {
			t.Errorf("message list lacks --%s", f)
		}
	}
	src, err := os.ReadFile("messageforward.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "a.deskHeld(ctx, c, model.ActionExtensionExecute") || !strings.Contains(string(src), "s.Hold(ctx, &p.Request.Command") {
		t.Error("message forward does not run through deskHeld and offer its write to the hold point")
	}
}

// TestSearchFilterIdentifiers: an empty identifier is a usage error.
func TestSearchFilterIdentifiers(t *testing.T) {
	c := parseFlags(t, "message list", "--message-id", " ")
	if _, err := searchFilter(c, desk.Filter{}); err == nil || !strings.Contains(err.Error(), "--message-id") {
		t.Fatalf("err = %v", err)
	}
	c = parseFlags(t, "message list", "--message-id", "<a@b.c>", "--provider-id", "gm1", "--references", "<o@b.c>")
	f, err := searchFilter(c, desk.Filter{})
	if err != nil || f.MessageID != "<a@b.c>" || f.ProviderID != "gm1" || f.References != "<o@b.c>" {
		t.Fatalf("filter = %+v, %v", f, err)
	}
}
