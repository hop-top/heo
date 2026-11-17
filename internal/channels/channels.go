// Package channels is what heo's channel and inbox commands do in the
// daemon, over the uma engine the daemon owns: find a channel or inbox
// by ID or key, plan a configuration change exactly as it would be
// submitted, submit it, and check channels without changing anything.
//
// A channel is one configured connection to a provider; its adapter is
// the implementation it runs. An inbox binds destinations of one channel
// to a selection policy, and carries the availability switch: a disabled
// inbox rejects inbound, while its channel keeps its connection and
// queued sends still deliver.
//
// Every call runs as the caller's uma client, so uma authorizes each one
// for the caller. Writes are uma configuration commands: committed
// operations keyed on the caller's operation ID, replacing a record only
// at the revision the caller names. A plan runs every check the
// submission makes that heo can make without writing — uma's request
// and adapter validation, key, destination and provider account
// uniqueness — so a preview that passes is the request that would be
// sent.
package channels

import (
	"context"
	"fmt"

	"hop.top/heo/internal/adapters"
	"hop.top/uma"
	"hop.top/uma/model"
)

// Correlation tags the configuration calls heo's commands make.
const Correlation model.CorrelationID = "heo-cli"

// scanPage is the page size of a scan over every channel or inbox.
const scanPage = 100

func call() model.Call { return model.Call{Correlation: Correlation} }

// Service is the configuration one caller reaches through the daemon's
// runtime.
type Service struct {
	// Engine validates channel configurations.
	Engine uma.Engine
	// Client is the caller's uma client: every read and write is
	// authorized for the caller.
	Client uma.Client
	// Tenant is the profile's tenant.
	Tenant model.TenantID
	// Adapters are the adapters this daemon runs.
	Adapters *adapters.Registry
	// Committed, when set, runs after each channel or inbox write is
	// submitted, whatever its outcome (a write whose answer was lost
	// may still have committed): the daemon applies what changed to its
	// receivers and webhook routes. Its error never fails the write.
	Committed func(context.Context) error
}

// committed runs s.Committed, if set.
func (s Service) committed(ctx context.Context) {
	if s.Committed != nil {
		_ = s.Committed(ctx)
	}
}

// ListChannels returns one page of the channels the caller may read.
func (s Service) ListChannels(ctx context.Context, page model.PageRequest) (model.Page[model.Channel], error) {
	return s.Client.Configuration().Channels(ctx, model.ListRequest{Call: call(), Page: page})
}

// ListInboxes returns one page of the inboxes the caller may read.
func (s Service) ListInboxes(ctx context.Context, page model.PageRequest) (model.Page[model.Inbox], error) {
	return s.Client.Configuration().Inboxes(ctx, model.ListRequest{Call: call(), Page: page})
}

// FindChannel returns the channel whose key or ID is ref: a key first,
// as uma resolves it, then an ID among the channels the caller may read.
func (s Service) FindChannel(ctx context.Context, ref string) (model.Channel, error) {
	if ref == "" {
		return model.Channel{}, model.NewError(model.Validation, "channel: an ID or key is required")
	}
	ch, err := s.Client.Configuration().ChannelByKey(ctx, model.LookupKey{Call: call(), Key: ref})
	if !model.IsCode(err, model.NotFound) && !model.IsCode(err, model.Validation) {
		return ch, err
	}
	all, err := s.allChannels(ctx)
	if err != nil {
		return model.Channel{}, err
	}
	for _, ch := range all {
		if string(ch.Ref.ID) == ref {
			return ch, nil
		}
	}
	return model.Channel{}, model.NewError(model.NotFound, fmt.Sprintf("channel %q: no channel has this key or ID", ref))
}

// FindInbox returns the inbox whose key or ID is ref: a key first, as
// uma resolves it, then an ID among the inboxes the caller may read.
func (s Service) FindInbox(ctx context.Context, ref string) (model.Inbox, error) {
	if ref == "" {
		return model.Inbox{}, model.NewError(model.Validation, "inbox: an ID or key is required")
	}
	in, err := s.Client.Configuration().InboxByKey(ctx, model.LookupKey{Call: call(), Key: ref})
	if !model.IsCode(err, model.NotFound) && !model.IsCode(err, model.Validation) {
		return in, err
	}
	if model.ID(ref).Validate() == nil {
		page, err := s.Client.Configuration().Inboxes(ctx, model.ListRequest{
			Call:   call(),
			Filter: model.Filter{Inboxes: []model.InboxRef{{Tenant: s.Tenant, ID: model.ID(ref)}}},
			Page:   model.PageRequest{Limit: 1},
		})
		if err != nil && !model.IsCode(err, model.Validation) {
			return model.Inbox{}, err
		}
		for _, in := range page.Items {
			if string(in.Ref.ID) == ref {
				return in, nil
			}
		}
	}
	return model.Inbox{}, model.NewError(model.NotFound, fmt.Sprintf("inbox %q: no inbox has this key or ID", ref))
}

// allChannels lists every channel the caller may read.
func (s Service) allChannels(ctx context.Context) ([]model.Channel, error) {
	var out []model.Channel
	page := model.PageRequest{Limit: scanPage}
	for {
		p, err := s.ListChannels(ctx, page)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if p.Complete || p.Next == "" {
			return out, nil
		}
		page.After = p.Next
	}
}

// allInboxes lists every inbox the caller may read.
func (s Service) allInboxes(ctx context.Context) ([]model.Inbox, error) {
	var out []model.Inbox
	page := model.PageRequest{Limit: scanPage}
	for {
		p, err := s.ListInboxes(ctx, page)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if p.Complete || p.Next == "" {
			return out, nil
		}
		page.After = p.Next
	}
}

// channelByID returns the channel with id among those the caller may
// read.
func (s Service) channelByID(ctx context.Context, id model.ID) (model.Channel, error) {
	all, err := s.allChannels(ctx)
	if err != nil {
		return model.Channel{}, err
	}
	for _, ch := range all {
		if ch.Ref.ID == id {
			return ch, nil
		}
	}
	return model.Channel{}, model.NewError(model.NotFound, fmt.Sprintf("channel %s: not found", id))
}

// Name is how output names a channel or inbox: its key when it has one,
// its ID otherwise.
func Name(key string, id model.ID) string {
	if key != "" {
		return key
	}
	return string(id)
}
