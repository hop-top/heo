package desk_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

func rawWorld(t *testing.T, read func(model.ExternalRef) ([]byte, error)) *world {
	t.Helper()
	return newWorldFake(t, func(f *adapterstest.Factory) { f.DeclareRaw(read) })
}

func wantReason(t *testing.T, err error, code model.ErrorCode, reason model.ErrorReason) {
	t.Helper()
	me, ok := err.(*model.Error)
	if !ok || me.Code != code || me.Reason != reason {
		t.Fatalf("err = %#v, want %s/%q", err, code, reason)
	}
}

// RawMessage returns the provider's bytes whole, with their size and
// media type, for a message the caller reads.
func TestRawMessageReturnsProviderBytes(t *testing.T) {
	w := rawWorld(t, nil)
	m := w.received("m1")
	got, err := w.as(agent).RawMessage(t.Context(), string(m.Ref.ID), "")
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	want := adapterstest.RawBytes(m.External[0].ID)
	if !bytes.Equal(got.Content, want) || got.Size != int64(len(want)) || got.MediaType != "message/rfc822" || got.Message != m.Ref.ID {
		t.Fatalf("raw = %+v, want %q", got, want)
	}
	// Narrowing the lookup to the message's own conversation reads it too.
	if _, err := w.as(agent).RawMessage(t.Context(), string(m.Ref.ID), string(m.Ref.Conversation.ID)); err != nil {
		t.Fatalf("raw --conversation: %v", err)
	}
}

// A channel whose adapter does not declare raw_read refuses (Unsupported).
func TestRawMessageUnsupported(t *testing.T) {
	w := newWorld(t)
	m := w.received("m1")
	_, err := w.as(agent).RawMessage(t.Context(), string(m.Ref.ID), "")
	wantReason(t, err, model.Unsupported, "")
}

// A copy over the adapter's bound is Permanent with the reason too_large,
// never truncated; a message the provider no longer holds is NotFound.
func TestRawMessageProviderRefusals(t *testing.T) {
	w := rawWorld(t, func(ref model.ExternalRef) ([]byte, error) {
		if ref.ID == "gone" {
			return nil, model.NewError(model.NotFound, "message not found at the provider")
		}
		return nil, model.NewReasonError(model.Permanent, model.TooLarge, "message is over the read bound")
	})
	big := w.received("big")
	_, err := w.as(agent).RawMessage(t.Context(), string(big.Ref.ID), "")
	wantReason(t, err, model.Permanent, model.TooLarge)
	gone := w.received("gone")
	_, err = w.as(agent).RawMessage(t.Context(), string(gone.Ref.ID), "")
	wantReason(t, err, model.NotFound, "")
}

// A redacted message keeps no provider copy served around its scrubbing:
// Conflict. An unknown message and one narrowed away from the caller are
// NotFound, as for a get.
func TestRawMessageAuthorizationAndState(t *testing.T) {
	w := rawWorld(t, nil)
	redacted := w.received("r1")
	w.redact(redacted)
	_, err := w.as(agent).RawMessage(t.Context(), string(redacted.Ref.ID), "")
	wantReason(t, err, model.Conflict, "")

	_, err = w.as(agent).RawMessage(t.Context(), "00000000-0000-7000-8000-00000000dead", "")
	wantReason(t, err, model.NotFound, "")

	hidden := w.received("h1")
	w.narrow("h1")
	if _, err := w.as(agent).RawMessage(t.Context(), string(hidden.Ref.ID), ""); err != nil {
		t.Fatalf("desk reads a narrowed message: %v", err)
	}
	_, err = w.as(viewer).RawMessage(t.Context(), string(hidden.Ref.ID), "")
	wantReason(t, err, model.NotFound, "")
}

// Served over REST, rpc or MCP the result is JSON: the content is base64
// beside its size.
func TestRawMessageJSONShape(t *testing.T) {
	b, err := json.Marshal(desk.RawMessage{Message: "m1", MediaType: "message/rfc822", Size: 5, Content: []byte("hello")})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"message":"m1","media_type":"message/rfc822","size":5,"content":"aGVsbG8="}` {
		t.Fatalf("json = %s", b)
	}
}
