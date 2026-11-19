package desk

import (
	"context"
	"fmt"
	"slices"

	"hop.top/uma/model"
)

// MessageItem is one provider message of a conversation: the heo message
// record and the provider's own ID for it, what a provider verb acts on.
type MessageItem struct {
	// Message is the message's record ID.
	Message string `json:"message"`
	// Provider is the provider's message ID.
	Provider string `json:"provider"`
}

// providerItems returns the conversation's provider messages on its
// channel, oldest first, as uma's state verbs load them: not deleted, not
// redacted, holding a provider message reference.
func (s Service) providerItems(ctx context.Context, conv model.Conversation) ([]MessageItem, error) {
	req := model.ListRequest{Call: call(), Filter: model.Filter{Conversation: &conv.Ref}, Page: model.PageRequest{Limit: scanPage}}
	var out []MessageItem
	for {
		p, err := s.Client.Messages().List(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, m := range p.Items {
			if m.Deleted || m.Redacted {
				continue
			}
			if id := providerMessageID(m, conv.ReplyRoute.Channel); id != "" {
				out = append(out, MessageItem{Message: string(m.Ref.ID), Provider: id})
			}
		}
		if p.Complete || p.Next == "" || p.Next == req.Page.After {
			return out, nil
		}
		req.Page.After = p.Next
	}
}

// providerMessageID is m's provider message ID on channel, empty when it
// holds none there.
func providerMessageID(m model.Message, channel model.ChannelRef) string {
	for _, x := range m.External {
		if x.Channel == channel && x.Namespace == model.ProviderMessageNamespace && x.Kind == model.RefKindMessage {
			return x.ID
		}
	}
	return ""
}

// TargetMessages resolves the message record IDs ids to the conversation's
// provider messages, in the conversation's order (the order uma applies a
// change in), each once. An ID that is not a
// message of the conversation (unknown, another conversation's, deleted,
// redacted, or with no provider message on its channel) is
// model.Validation: nothing is planned, so nothing is applied.
func (s Service) TargetMessages(ctx context.Context, conv model.Conversation, ids []string) ([]MessageItem, error) {
	have, err := s.providerItems(ctx, conv)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if !slices.ContainsFunc(have, func(h MessageItem) bool { return h.Message == id }) {
			return nil, model.NewError(model.Validation, fmt.Sprintf(
				"--message %s: not a message of conversation %s that its provider holds (`heo message list --conversation %s` lists them)",
				id, conv.Ref.ID, conv.Ref.ID))
		}
	}
	return slices.DeleteFunc(have, func(h MessageItem) bool { return !slices.Contains(ids, h.Message) }), nil
}

// uniqueStrings returns ids without repeats, in order.
func uniqueStrings(ids []string) []string {
	var out []string
	for _, id := range ids {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}
