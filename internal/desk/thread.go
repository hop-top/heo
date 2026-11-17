package desk

import (
	"context"

	"hop.top/uma/model"
)

// ThreadWindow bounds how many of a conversation's newest messages
// Thread keeps: a thread longer than this is read to its end and only
// its tail is returned, so memory stays bounded while the newest
// messages, the ones a summary needs most, are never the ones dropped.
const ThreadWindow = 2000

// Thread is the messages of one conversation as the caller may read
// them, oldest first, in the order uma recorded them.
type Thread struct {
	Conversation model.Conversation
	// Messages are the caller's visible, undeleted messages: the tail
	// of the thread when Cut is set.
	Messages []model.Message
	// Total is how many such messages the conversation holds.
	Total int
	// Cut is set when Messages is only the newest ThreadWindow of
	// Total.
	Cut bool
	// Notes is set when internal notes are among Messages.
	Notes bool
}

// SeesNotes reports whether the caller may read internal notes.
func (s Service) SeesNotes() bool {
	return s.seesAudience(model.Audience{Visibility: model.Internal})
}

// Thread reads the conversation with record ID id, as the caller. Every
// message is read through uma, which authorizes it for the caller, and
// passes heo's audience rule again; internal notes are returned only
// when notes is set and the caller may read them, so a caller who may
// not never has one in its hands. Nothing is written.
func (s Service) Thread(ctx context.Context, id string, notes bool) (Thread, error) {
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return Thread{}, err
	}
	t := Thread{Conversation: conv, Notes: notes && s.SeesNotes()}
	req := model.ListRequest{
		Call:   call(),
		Filter: model.Filter{Conversation: &conv.Ref},
		Page:   model.PageRequest{Limit: scanPage},
	}
	for {
		p, err := s.Client.Messages().List(ctx, req)
		if err != nil {
			return Thread{}, err
		}
		for _, m := range s.visible(p.Items, false) {
			if m.Deleted || IsNote(m) && !t.Notes {
				continue
			}
			t.Total++
			t.Messages = append(t.Messages, m)
			if len(t.Messages) > 2*ThreadWindow {
				t.Messages = append([]model.Message(nil), t.Messages[len(t.Messages)-ThreadWindow:]...)
			}
		}
		if p.Complete || p.Next == "" {
			break
		}
		req.Page.After = p.Next
	}
	if len(t.Messages) > ThreadWindow {
		t.Messages = t.Messages[len(t.Messages)-ThreadWindow:]
	}
	t.Cut = t.Total > len(t.Messages)
	return t, nil
}

// maxLabels bounds the label definitions AllLabels reads.
const maxLabels = 1000

// AllLabels returns the tenant's label definitions the caller may read,
// up to maxLabels, in the order they were created.
func (s Service) AllLabels(ctx context.Context) ([]model.Label, error) {
	var out []model.Label
	page := model.PageRequest{Limit: scanPage}
	for len(out) < maxLabels {
		p, err := s.ListLabels(ctx, page)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if p.Complete || p.Next == "" {
			break
		}
		page.After = p.Next
	}
	return out, nil
}
