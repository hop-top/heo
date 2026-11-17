package desk

import (
	"context"
	"fmt"

	"hop.top/uma/model"
)

// RawMessage is a message's whole provider copy: for mail, the RFC 5322
// message with every header and part, byte for byte as the provider
// serves it. uma keeps none of it; each read goes to the provider.
type RawMessage struct {
	// Message is the message's record ID.
	Message   model.ID `json:"message"    yaml:"message"`
	MediaType string   `json:"media_type" yaml:"media_type"`
	Size      int64    `json:"size"       yaml:"size"`
	// Content is the copy, base64 in JSON.
	Content []byte `json:"content" yaml:"content"`
}

// RawMessage reads the provider's copy of message id (in conversation
// conv, record ID, when set). The caller must be allowed to read the
// message, as for a get; uma authorizes the read again under the
// message's audience. The channel's adapter must declare raw_read
// (model.Unsupported otherwise), a copy over the adapter's bound is
// model.Permanent with the reason too_large, a redacted or deleted
// message, or one with no provider copy, is model.Conflict. A read:
// nothing changes at the provider, and it may be repeated.
func (s Service) RawMessage(ctx context.Context, id, conv string) (RawMessage, error) {
	m, err := s.FindMessage(ctx, id, conv, false)
	if err != nil {
		return RawMessage{}, err
	}
	raw, err := s.Client.Messages().Raw(ctx, model.Lookup[model.MessageRef]{Call: call(), Ref: m.Ref})
	if err != nil {
		return RawMessage{}, err
	}
	if int64(len(raw.Bytes)) != raw.Size {
		return RawMessage{}, fmt.Errorf("message %s: the provider copy does not match its size", id)
	}
	return RawMessage{Message: m.Ref.ID, MediaType: raw.MediaType, Size: raw.Size, Content: raw.Bytes}, nil
}
