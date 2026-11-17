package desk

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/channels"
	"hop.top/uma/adapter/mail/mailboxes"
	"hop.top/uma/model"
)

// Provider labels. A mail provider keeps mailboxes of its own: a Gmail
// label, an Exchange folder or category, an IMAP mailbox. They are not
// heo's labels (`label`, uma's tenant labels, local to uma): the mail
// adapters list and create them through two extensions of their type,
// `<type>.mailboxes` (a read, Extensions.Query) and `<type>.mailbox_create`
// (an effect, Extensions.Execute, held for a plugin actor like any write).
// A channel can do each only where its adapter declares extension_query or
// extension_execute and the extension's own capability, ships the
// definition, and reports the capability available now.

// ProviderLabel is one mailbox of a provider account, on a channel.
type ProviderLabel struct {
	// Channel is the channel's key, its ID when it has none.
	Channel string `json:"channel" yaml:"channel"`
	// ID is the provider's own: a Gmail label ID, an Exchange folder ID,
	// an IMAP mailbox name as the server spells it. The verbs that file
	// or tag take it.
	ID string `json:"id" yaml:"id"`
	// Name is the display name, "/" separating a hierarchy.
	Name string `json:"name" yaml:"name"`
	// Kind is label, folder or category.
	Kind string `json:"kind" yaml:"kind"`
	// Role is the special use the provider gives it, empty for most.
	Role string `json:"role" yaml:"role"`
}

// reachable returns the channels the caller reaches through the inboxes
// it may read, narrowed to the one whose key or ID is ref when set, by
// name. A desk actor may not read channels, so the runtime lists them
// (Channels) and the caller's inboxes say which are its own.
func (s Service) reachable(ctx context.Context, ref string) ([]model.Channel, error) {
	if s.Channels == nil {
		return nil, model.NewError(model.Unsupported, "provider labels: the runtime's channels are unknown here")
	}
	all, err := s.Channels(ctx)
	if err != nil {
		return nil, err
	}
	own := map[model.ID]bool{}
	page := model.PageRequest{Limit: scanPage}
	for {
		p, err := s.inboxes().ListInboxes(ctx, page)
		if err != nil {
			return nil, err
		}
		for _, in := range p.Items {
			own[in.Channel.ID] = true
		}
		if p.Complete || p.Next == "" {
			break
		}
		page.After = p.Next
	}
	var out []model.Channel
	for _, ch := range all {
		if own[ch.Ref.ID] && (ref == "" || ch.Key == ref || string(ch.Ref.ID) == ref) {
			out = append(out, ch)
		}
	}
	if ref != "" && len(out) == 0 {
		return nil, model.NewError(model.NotFound, fmt.Sprintf("channel %q: no channel with this key or ID that you reach through an inbox", ref))
	}
	slices.SortFunc(out, func(a, b model.Channel) int {
		return strings.Compare(channels.Name(a.Key, a.Ref.ID), channels.Name(b.Key, b.Ref.ID))
	})
	return out, nil
}

// mailboxGate checks that channel ch can run the mailbox extension name,
// the read (write false) or the effect (write true), for verb: the
// adapter declares extension_query or extension_execute and the
// extension's own capability, ships the definition, and reports both
// available now. Anything else is a refusal naming what is missing;
// where the capability is declared but unavailable the code is the one
// the channel reports.
func (s Service) mailboxGate(ctx context.Context, ch model.Channel, verb, name string, write bool) error {
	base := model.CapabilityExtensionQuery
	if write {
		base = model.CapabilityExtensionExecute
	}
	own := model.Capability(name)
	label := channels.Name(ch.Key, ch.Ref.ID)
	if s.Adapter == nil || s.Effects == nil {
		return model.NewError(model.Unsupported, fmt.Sprintf("channel %s: its adapter is unknown here", label))
	}
	d, err := s.Adapter(ctx, ch.Ref)
	if err != nil {
		return err
	}
	for _, c := range []model.Capability{base, own} {
		if !adapters.Declares(d, c) {
			return model.NewError(model.Unsupported, fmt.Sprintf(
				"channel %s: adapter %s does not declare %s, which `%s` needs; `heo channel get %s` lists what the channel supports",
				label, d.Type, c, verb, ch.Ref.ID))
		}
	}
	defs, err := s.Effects(ctx, ch.Ref)
	if err != nil {
		return err
	}
	if !adapters.ShipsEffect(defs, name, own) {
		return model.NewError(model.Unsupported, fmt.Sprintf(
			"channel %s: adapter %s ships no extension %s, which `%s` needs", label, d.Type, name, verb))
	}
	if s.Capabilities == nil {
		return nil
	}
	statuses, err := s.Capabilities(ctx, ch.Ref)
	if err != nil {
		return err
	}
	for _, c := range []model.Capability{base, own} {
		i := slices.IndexFunc(statuses, func(st model.CapabilityStatus) bool { return st.Name == c })
		if i < 0 {
			return model.NewError(model.Unsupported, fmt.Sprintf("channel %s: its adapter does not report %s, which `%s` needs", label, c, verb))
		}
		if st := statuses[i]; !st.Available {
			code := st.Reason
			if code == "" {
				code = model.Unavailable
			}
			return model.NewError(code, fmt.Sprintf("channel %s: %s is %s now, so `%s` is refused", label, c, code, verb))
		}
	}
	return nil
}

// refused reports whether err is a gate's refusal that leaves a channel
// out of a listing without failing it: what the adapter does not have or
// does not offer now.
func refused(err error) bool {
	return model.IsCode(err, model.Unsupported) || model.IsCode(err, model.Disabled) || model.IsCode(err, model.Unavailable) ||
		model.IsCode(err, model.Unauthorized)
}

// readMailboxes lists the mailboxes of channel ch (the caller's read of
// the adapter's `<type>.mailboxes`: extension.query). The gate was
// checked by the caller.
func (s Service) readMailboxes(ctx context.Context, ch model.Channel) ([]model.ProviderMailbox, error) {
	res, err := s.Client.Extensions().Query(ctx, model.QueryExtension{
		Call: call(), Target: ch.Ref.Ref(), Audience: effectAudience,
		Query: model.Extension{Name: model.MailboxListName(ch.Type), Version: model.MailboxExtensionVersion},
	})
	if err != nil {
		return nil, err
	}
	return model.DecodeProviderMailboxes(res)
}

// ListProviderLabels lists the provider mailboxes of the channels the
// caller reaches, in a stable order: channel name, kind, name, ID. With
// channel (key or ID) it lists that channel's alone and refuses it where
// it cannot list (model.Unsupported, or the code the channel reports for
// an unavailable capability); without, a channel that cannot list is
// left out. Reading is the caller's extension.query.
func (s Service) ListProviderLabels(ctx context.Context, channel string) ([]ProviderLabel, error) {
	chs, err := s.reachable(ctx, channel)
	if err != nil {
		return nil, err
	}
	out := []ProviderLabel{}
	for _, ch := range chs {
		if err := s.mailboxGate(ctx, ch, "provider-label list", model.MailboxListName(ch.Type), false); err != nil {
			if channel == "" && refused(err) {
				continue
			}
			return nil, err
		}
		boxes, err := s.readMailboxes(ctx, ch)
		if err != nil {
			return nil, err
		}
		name := channels.Name(ch.Key, ch.Ref.ID)
		for _, b := range boxes {
			out = append(out, ProviderLabel{Channel: name, ID: b.ID, Name: b.Name, Kind: b.Kind, Role: b.Role})
		}
	}
	slices.SortStableFunc(out, func(a, b ProviderLabel) int {
		for _, c := range []int{strings.Compare(a.Channel, b.Channel), strings.Compare(a.Kind, b.Kind), strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID)} {
			if c != 0 {
				return c
			}
		}
		return 0
	})
	return out, nil
}

// ProviderLabelPlan is a validated create of a provider mailbox: the
// effect on the channel and, from a preview, what was checked of the
// adapter and what the provider holds under that name already.
type ProviderLabelPlan struct {
	Request model.ExecuteExtension
	Channel model.Channel
	Name    string
	// Kind is the kind asked, empty for the adapter's default.
	Kind string
	// Declaration is set by a preview only.
	Declaration *EffectDeclaration
	// Existing is the mailbox the provider holds under the name, set by
	// a preview that listed it: the create is then a no-op at the
	// provider, which is idempotent by name.
	Existing *ProviderLabel
}

// PlanProviderLabelCreate builds and checks the create of the provider
// mailbox name (kind, when set, is the adapter's; empty is its default)
// on channel (key or ID), under operation op. It writes nothing. Without
// channel the one channel the caller reaches that can create is used:
// none is model.Unsupported, several are model.Validation naming them.
// The channel's adapter must declare extension_execute and the
// `<type>.mailbox_create` capability and ship the effect, in a preview and
// on submission; a preview also asks the adapter, offline, to check the
// fields and reads the list, to say whether the name exists.
func (s Service) PlanProviderLabelCreate(ctx context.Context, name, kind, channel string, op model.OperationID, mode channels.Mode) (ProviderLabelPlan, error) {
	c := model.CreateProviderMailbox{Name: name, Kind: kind}
	if err := c.Validate(); err != nil {
		return ProviderLabelPlan{}, err
	}
	chs, err := s.reachable(ctx, channel)
	if err != nil {
		return ProviderLabelPlan{}, err
	}
	var able []model.Channel
	var gate error
	for _, ch := range chs {
		if err := s.mailboxGate(ctx, ch, "provider-label create", model.MailboxCreateName(ch.Type), true); err != nil {
			if channel == "" && refused(err) {
				gate = err
				continue
			}
			return ProviderLabelPlan{}, err
		}
		able = append(able, ch)
	}
	switch {
	case len(able) == 0 && gate != nil:
		return ProviderLabelPlan{}, gate
	case len(able) == 0:
		return ProviderLabelPlan{}, model.NewError(model.Unsupported, "provider-label create: no channel you reach has an adapter that creates provider mailboxes")
	case len(able) > 1:
		names := make([]string, len(able))
		for i, ch := range able {
			names[i] = channels.Name(ch.Key, ch.Ref.ID)
		}
		return ProviderLabelPlan{}, model.NewError(model.Validation, fmt.Sprintf(
			"channel: %d channels can create provider mailboxes (%s); name one with --channel", len(able), strings.Join(names, ", ")))
	}
	ch := able[0]
	ext, err := c.Extension(ch.Type)
	if err != nil {
		return ProviderLabelPlan{}, err
	}
	req := model.ExecuteExtension{
		Command: model.Command{Call: call(), Operation: op}, Target: ch.Ref.Ref(), Audience: effectAudience, Action: ext,
	}
	if err := req.Validate(); err != nil {
		return ProviderLabelPlan{}, err
	}
	p := ProviderLabelPlan{Request: req, Channel: ch, Name: name, Kind: kind}
	if mode == channels.Preview {
		p.Declaration = &EffectDeclaration{
			Declaration: Declaration{Capability: model.CapabilityExtensionExecute, Adapter: ch.Type, Channel: ch.Ref},
			Effect:      model.MailboxCreateName(ch.Type),
		}
		if err := s.validateAction(ctx, ch.Ref, req.Audience, req.Action); err != nil {
			return ProviderLabelPlan{}, err
		}
		if s.mailboxGate(ctx, ch, "provider-label list", model.MailboxListName(ch.Type), false) == nil {
			if boxes, err := s.readMailboxes(ctx, ch); err == nil {
				if b, ok := findMailbox(boxes, name, kind); ok {
					p.Existing = &ProviderLabel{Channel: channels.Name(ch.Key, ch.Ref.ID), ID: b.ID, Name: b.Name, Kind: b.Kind, Role: b.Role}
				}
			}
		}
	}
	return p, nil
}

// findMailbox is the mailbox a create of name (kind when set) would
// find: the provider compares names without regard to case.
func findMailbox(boxes []model.ProviderMailbox, name, kind string) (model.ProviderMailbox, bool) {
	if kind != "" {
		return mailboxes.Find(boxes, name, kind)
	}
	for _, b := range boxes {
		if strings.EqualFold(b.Name, name) {
			return b, true
		}
	}
	return model.ProviderMailbox{}, false
}

// CreateProviderLabel submits p: uma queues the create durably, and the
// daemon's workers send it to the provider. The mailbox exists once the
// operation settled; its provider reference (the mailbox ID) is among the
// operation's provider references then.
func (s Service) CreateProviderLabel(ctx context.Context, p ProviderLabelPlan) (model.Acceptance, error) {
	return s.Client.Extensions().Execute(ctx, p.Request)
}

// Resolved is one provider mailbox name a verb resolved to the ID the
// provider files by.
type Resolved struct {
	// Given is what the caller wrote, ID the mailbox ID it stands for.
	Given, ID string
	// Name is the mailbox's display name.
	Name string
}

// resolveMailboxes resolves values, each a provider mailbox ID or name,
// to IDs through the channel's mailbox list: a value that is the ID of a
// mailbox is that ID (an ID wins over a name that spells like one), else
// the mailbox of that name, exactly spelled, else without regard to case.
// A name two mailboxes hold at the same tier is model.Validation naming
// their IDs: nothing is guessed. A value no mailbox holds is left as
// given, for the provider to answer, as before names resolved. It
// resolves nothing where the channel cannot list (its adapter lacks the
// extension, or it is unavailable now), so a channel without the list
// takes IDs only, as it always did; a list that fails to read is the
// caller's error.
func (s Service) resolveMailboxes(ctx context.Context, in model.Inbox, values []string) ([]string, []Resolved, error) {
	if len(values) == 0 || s.Adapter == nil || s.Effects == nil || s.Capabilities == nil {
		return values, nil, nil
	}
	d, err := s.Adapter(ctx, in.Channel)
	if err != nil {
		return values, nil, nil //nolint:nilerr // an unknown adapter lists nothing: the values go as given
	}
	ch := model.Channel{Ref: in.Channel, Type: d.Type}
	if s.mailboxGate(ctx, ch, "provider mailbox name", model.MailboxListName(d.Type), false) != nil {
		return values, nil, nil
	}
	boxes, err := s.readMailboxes(ctx, ch)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving provider mailbox names: %w", err)
	}
	out := make([]string, len(values))
	var res []Resolved
	for i, v := range values {
		id, name, err := pickMailbox(boxes, v)
		if err != nil {
			return nil, nil, err
		}
		out[i] = id
		if id != v {
			res = append(res, Resolved{Given: v, ID: id, Name: name})
		}
	}
	return out, res, nil
}

// pickMailbox is the ID v stands for among boxes, and the name of the
// mailbox; v itself when no mailbox holds it.
func pickMailbox(boxes []model.ProviderMailbox, v string) (id, name string, err error) {
	if slices.ContainsFunc(boxes, func(b model.ProviderMailbox) bool { return b.ID == v }) {
		return v, "", nil
	}
	for _, same := range []func(a, b string) bool{func(a, b string) bool { return a == b }, strings.EqualFold} {
		var hit []model.ProviderMailbox
		for _, b := range boxes {
			if same(b.Name, v) {
				hit = append(hit, b)
			}
		}
		switch len(hit) {
		case 0:
			continue
		case 1:
			return hit[0].ID, hit[0].Name, nil
		}
		ids := make([]string, len(hit))
		for i, b := range hit {
			ids[i] = b.Kind + " " + b.ID
		}
		return "", "", model.NewError(model.Validation, fmt.Sprintf(
			"provider mailbox %q: %d mailboxes have this name (%s); name the ID (`heo provider-label list` shows them)", v, len(hit), strings.Join(ids, ", ")))
	}
	return v, "", nil
}

// TrustChannel resolves what a held write's Channel names to the channel
// uma scopes its trust counters to: the channel of an inbox, the usual
// case, or, for a write on a channel itself (a provider mailbox create),
// the channel named by its ID. An inbox is looked for first.
func (s Service) TrustChannel(ctx context.Context, name string) (model.ChannelRef, error) {
	in, err := s.FindInbox(ctx, name)
	if model.IsCode(err, model.NotFound) && model.ID(name).Validate() == nil {
		return model.ChannelRef{Tenant: s.Tenant, ID: model.ID(name)}, nil
	}
	return in.Channel, err
}
