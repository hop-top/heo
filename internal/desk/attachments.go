package desk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"hop.top/uma/model"
)

// Attachment is what the caller learns of an attachment it may read:
// the metadata uma recorded, never its bytes.
type Attachment struct {
	// Ref names the attachment, <inbox>/<attachment>, the form the
	// commands take.
	Ref       string         `json:"ref"        yaml:"ref"`
	Inbox     model.ID       `json:"inbox"      yaml:"inbox"`
	ID        model.ID       `json:"id"         yaml:"id"`
	Name      string         `json:"name"       yaml:"name"`
	MediaType string         `json:"media_type" yaml:"media_type"`
	Size      int64          `json:"size"       yaml:"size"`
	Digest    string         `json:"digest"     yaml:"digest"`
	Audience  model.Audience `json:"audience"   yaml:"audience"`
	// Messages are the messages that hold the attachment, among those
	// the caller reads in the listing; empty where a lookup by ref
	// does not ask.
	Messages []model.ID `json:"messages,omitempty" yaml:"messages,omitempty"`
}

func attachmentOf(m model.Attachment) Attachment {
	ref := m.Ref
	return Attachment{
		Ref: RefName(ref), Inbox: ref.Inbox.ID, ID: ref.ID,
		Name: m.Metadata.Name, MediaType: m.Metadata.MediaType, Size: m.Metadata.Size,
		Digest: m.Metadata.Digest, Audience: m.Metadata.Audience,
	}
}

// RefName is the form commands take an attachment in:
// <inbox>/<attachment>, both record IDs.
func RefName(r model.AttachmentRef) string { return string(r.Inbox.ID) + "/" + string(r.ID) }

// ParseRef reads an attachment ref in the form RefName prints, in the
// profile's tenant. uma scopes an attachment to its inbox, and an
// attachment no message holds cannot be found by scanning messages, so
// the ref names both. A malformed ref is a usage error; IDs that are not
// record IDs name nothing.
func (s Service) ParseRef(ref string) (model.AttachmentRef, error) {
	inbox, id, ok := strings.Cut(ref, "/")
	if !ok || inbox == "" || id == "" || strings.Contains(id, "/") {
		return model.AttachmentRef{}, model.NewError(model.Validation, fmt.Sprintf(
			"attachment %q: want <inbox>/<attachment> (record IDs, as `attachment list` prints it)", ref))
	}
	for _, p := range []string{inbox, id} {
		if model.ID(p).Validate() != nil {
			return model.AttachmentRef{}, model.NewError(model.NotFound, fmt.Sprintf("attachment %s: not found", ref))
		}
	}
	return model.AttachmentRef{Inbox: model.InboxRef{Tenant: s.Tenant, ID: model.ID(inbox)}, ID: model.ID(id)}, nil
}

// heldBy returns the attachment refs m's parts name, in order, once each.
func heldBy(m model.Message) []model.AttachmentRef {
	var out []model.AttachmentRef
	for _, p := range m.Content.Parts {
		if p.Kind == model.PartAttachment && p.Attachment != nil && !slices.Contains(out, *p.Attachment) {
			out = append(out, *p.Attachment)
		}
	}
	return out
}

// ListAttachments returns one page of the attachments of the messages
// the caller may read: of the message msg (record ID) when it is set,
// else of f's conversation. A page covers the messages uma returned for
// page, and continues from next; a page whose messages hold none is
// skipped, so an incomplete page is never empty. An attachment the
// caller may not read, or whose bytes are not stored, is not listed.
// The metadata is read with Attachments.Open and the stream closed
// unread: uma has no metadata-only read.
func (s Service) ListAttachments(ctx context.Context, f Filter, msg string, page model.PageRequest) (model.Page[Attachment], error) {
	out := model.Page[Attachment]{Items: []Attachment{}}
	if msg != "" {
		m, err := s.FindMessage(ctx, msg, f.Conversation, false)
		if err != nil {
			return out, err
		}
		out.Items, err = s.describe(ctx, []model.Message{m})
		out.Complete = true
		return out, err
	}
	filter, err := s.messageFilter(ctx, f)
	if err != nil {
		return out, err
	}
	req := model.ListRequest{Call: call(), Filter: filter, Page: page}
	for {
		p, err := s.Client.Messages().List(ctx, req)
		if err != nil {
			return out, err
		}
		if out.Items, err = s.describe(ctx, s.visible(p.Items, false)); err != nil {
			return out, err
		}
		out.Next, out.Complete = p.Next, p.Complete
		if p.Complete || p.Next == "" || len(out.Items) > 0 {
			return out, nil
		}
		req.Page.After = p.Next
	}
}

// describe lists the attachments ms hold, each with the messages of ms
// that hold it.
func (s Service) describe(ctx context.Context, ms []model.Message) ([]Attachment, error) {
	items := []Attachment{}
	index := map[model.AttachmentRef]int{}
	for _, m := range ms {
		for _, ref := range heldBy(m) {
			i, seen := index[ref]
			if !seen {
				meta, rc, err := s.Client.Attachments().Open(ctx, model.Lookup[model.AttachmentRef]{Call: call(), Ref: ref})
				switch {
				case err == nil:
					_ = rc.Close()
				case unreadable(err):
					// Not the caller's to see, or without bytes: not listed.
					index[ref] = -1
					continue
				default:
					return nil, err
				}
				i = len(items)
				index[ref] = i
				items = append(items, attachmentOf(meta))
			}
			if i >= 0 {
				items[i].Messages = append(items[i].Messages, m.Ref.ID)
			}
		}
	}
	return items, nil
}

// unreadable reports whether err says the caller may not read an
// attachment, or that it has no bytes to read.
func unreadable(err error) bool {
	return model.IsCode(err, model.Unauthorized) || model.IsCode(err, model.NotFound) || model.IsCode(err, model.Conflict)
}

// GetAttachment returns the metadata of the attachment ref, which the
// caller must be allowed to open: uma authorizes it against the
// attachment's audience and the messages that hold it, now.
func (s Service) GetAttachment(ctx context.Context, ref string) (Attachment, error) {
	meta, rc, err := s.open(ctx, ref)
	if err != nil {
		return Attachment{}, err
	}
	_ = rc.Close()
	return attachmentOf(meta), nil
}

// OpenAttachment opens the attachment ref for reading, as uma
// authorizes it now. The caller owns, and must close, the reader.
func (s Service) OpenAttachment(ctx context.Context, ref string) (Attachment, io.ReadCloser, error) {
	meta, rc, err := s.open(ctx, ref)
	if err != nil {
		return Attachment{}, nil, err
	}
	return attachmentOf(meta), rc, nil
}

func (s Service) open(ctx context.Context, name string) (model.Attachment, io.ReadCloser, error) {
	ref, err := s.ParseRef(name)
	if err != nil {
		return model.Attachment{}, nil, err
	}
	meta, rc, err := s.Client.Attachments().Open(ctx, model.Lookup[model.AttachmentRef]{Call: call(), Ref: ref})
	if err != nil {
		return model.Attachment{}, nil, s.openError(ctx, ref, err)
	}
	return meta, rc, nil
}

// openError explains a refused open. A redacted attachment keeps no
// bytes: uma answers its open with a conflict while a message the caller
// may read still names it, and with unauthorized once none does, as it
// answers an attachment narrowed away from the caller. The first is
// gone; so is the second for a caller who reads content of every
// audience and finds no message holding the attachment. Any other caller
// cannot tell the second from narrowing, and is not told more than uma
// said.
func (s Service) openError(ctx context.Context, ref model.AttachmentRef, err error) error {
	gone := model.NewError(model.NotFound, fmt.Sprintf(
		"attachment %s: gone: its bytes are no longer stored (redacted, deleted or never uploaded)", RefName(ref)))
	switch {
	case model.IsCode(err, model.Conflict):
		return gone
	case !model.IsCode(err, model.Unauthorized) || !s.seesEverything():
		return err
	}
	if held, herr := s.held(ctx, ref); herr != nil || held {
		return err
	}
	return gone
}

// seesEverything reports whether the caller reads content of every
// audience, as a desk actor does.
func (s Service) seesEverything() bool {
	if s.Sees == nil {
		return false
	}
	for _, v := range []model.Visibility{model.Public, model.Internal, model.Restricted} {
		if !s.Sees(model.Audience{Visibility: v}) {
			return false
		}
	}
	return true
}

// held reports whether a message the caller may read holds ref.
func (s Service) held(ctx context.Context, ref model.AttachmentRef) (bool, error) {
	req := model.ListRequest{Call: call(), Filter: model.Filter{Inboxes: []model.InboxRef{ref.Inbox}}, Page: model.PageRequest{Limit: scanPage}}
	for {
		p, err := s.Client.Messages().List(ctx, req)
		if err != nil {
			return false, err
		}
		for _, m := range s.visible(p.Items, false) {
			if slices.Contains(heldBy(m), ref) {
				return true, nil
			}
		}
		if p.Complete || p.Next == "" {
			return false, nil
		}
		req.Page.After = p.Next
	}
}

// ReadAll reads an opened attachment to its end, refusing more than
// meta.Size bytes: uma verifies the length it recorded, and a stream
// past it is not trusted to end.
func ReadAll(a Attachment, rc io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(rc, a.Size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != a.Size {
		return nil, errors.New("attachment bytes do not match the recorded size")
	}
	return b, nil
}
