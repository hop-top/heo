package desk

import (
	"context"
	"encoding/json"
	"net/mail"
	"slices"
	"strings"

	"hop.top/uma/model"
)

// maxFactPages bounds the scan for a conversation's last inbound message.
const maxFactPages = 5

// replyToDiffers reports whether the last inbound message of conv
// carries a Reply-To that is not its From. The headers are what a mail
// adapter recorded with the message (a "reply_to" list in its payload);
// a channel that records none, or a conversation with no inbound
// message, never differs.
func (s Service) replyToDiffers(ctx context.Context, conv model.ConversationRef) (bool, error) {
	last, err := s.lastInbound(ctx, conv)
	return last != nil && ReplyToDiffers(*last), err
}

// lastInbound is the last inbound message of conv, nil when it has none.
func (s Service) lastInbound(ctx context.Context, conv model.ConversationRef) (*model.Message, error) {
	var last *model.Message
	page := model.PageRequest{Limit: 100}
	for range maxFactPages {
		p, err := s.ListMessages(ctx, Filter{Conversation: string(conv.ID)}, page)
		if err != nil {
			return nil, err
		}
		for i := range p.Items {
			m := p.Items[i]
			if inbound(m) && (last == nil || !m.OccurredAt.Before(last.OccurredAt)) {
				last = &m
			}
		}
		if p.Next == "" {
			break
		}
		page.After = p.Next
	}
	return last, nil
}

// outbound reports whether m was sent through uma to the other side.
func outbound(m model.Message) bool {
	return m.Audience.Visibility == model.Public && m.Origin != nil
}

// firstReplyUnknown computes the fact a reply to conv is held for under
// the ladder's first-reply policy: heo has never written to the
// contact it answers on this inbox. The contact is the one behind the
// last inbound message's identity; a prior write is any message sent
// through uma, in any conversation of this inbox that has one of the
// contact's identities there as a participant, the conversation being
// answered included. A conversation with no inbound message was opened
// from this side: the contact has been written to.
//
// The facts are read from uma's records as the caller may read them, so
// what the caller cannot read counts as not written: the answer errs
// towards holding, and so does a scan that reaches its bound.
func (s Service) firstReplyUnknown(ctx context.Context, conv model.Conversation) (bool, error) {
	last, err := s.lastInbound(ctx, conv.Ref)
	if err != nil {
		return false, err
	}
	if last == nil {
		return false, nil
	}
	id, err := s.FindIdentity(ctx, string(last.Author.Identity.ID))
	if model.IsCode(err, model.NotFound) || model.IsCode(err, model.Unauthorized) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	all, err := s.ContactIdentities(ctx, id.Contact)
	if err != nil {
		return false, err
	}
	mine := map[model.ID]bool{id.Ref.ID: true}
	for _, i := range all {
		if i.Ref.Inbox == conv.Ref.Inbox {
			mine[i.Ref.ID] = true
		}
	}
	page := model.PageRequest{Limit: 100}
	for range maxFactPages {
		p, err := s.ListConversations(ctx, Filter{Inbox: string(conv.Ref.Inbox.ID)}, page)
		if err != nil {
			return false, err
		}
		for _, c := range p.Items {
			if !slices.ContainsFunc(c.Participants, func(x model.Participant) bool { return mine[x.Identity.ID] }) {
				continue
			}
			sent, err := s.sentIn(ctx, c.Ref)
			if err != nil || sent {
				return !sent, err
			}
		}
		if p.Complete || p.Next == "" {
			return true, nil
		}
		page.After = p.Next
	}
	return true, nil
}

// sentIn reports whether any message of conv was sent through uma.
func (s Service) sentIn(ctx context.Context, conv model.ConversationRef) (bool, error) {
	page := model.PageRequest{Limit: 100}
	for range maxFactPages {
		p, err := s.ListMessages(ctx, Filter{Conversation: string(conv.ID)}, page)
		if err != nil {
			return false, err
		}
		if slices.ContainsFunc(p.Items, outbound) {
			return true, nil
		}
		if p.Next == "" {
			return false, nil
		}
		page.After = p.Next
	}
	return false, nil
}

// inbound reports whether m arrived from the other side: public, not
// authored through uma and not from a system actor.
func inbound(m model.Message) bool {
	return m.Audience.Visibility == model.Public && m.Origin == nil && m.Author.Kind != model.AuthorKindSystem
}

// ReplyToDiffers reports whether m names a Reply-To address that is not
// its sender's address.
func ReplyToDiffers(m model.Message) bool {
	from := address(m.Author.External.ID)
	for _, x := range m.Metadata {
		var p struct {
			ReplyTo []json.RawMessage `json:"reply_to"`
		}
		if json.Unmarshal(x.Data, &p) != nil {
			continue
		}
		for _, raw := range p.ReplyTo {
			var a string
			var o struct {
				Address string `json:"address"`
			}
			switch {
			case json.Unmarshal(raw, &a) == nil:
			case json.Unmarshal(raw, &o) == nil:
				a = o.Address
			}
			if a = address(a); a != "" && a != from {
				return true
			}
		}
	}
	return false
}

// address is the bare, lower-cased address of s ("Ada <A@x.org>" is
// "a@x.org"), or s trimmed and lower-cased when it does not parse.
func address(s string) string {
	if a, err := mail.ParseAddress(s); err == nil {
		s = a.Address
	}
	return strings.ToLower(strings.TrimSpace(s))
}
