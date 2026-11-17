package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/uma/model"
)

// inbound is one channel of the profile and how it receives.
type inbound struct {
	ch model.Channel
	// ad is the channel's adapter; known is false when this build
	// lacks it.
	ad    adapters.Adapter
	known bool
	// account is the provider account a receiving channel connects
	// to; accountErr is why it has none.
	account    string
	accountErr error
	// claimErr is why a channel added or changed while the daemon runs
	// holds no claim on its account: another channel of the profile, or
	// another daemon on this host, receives it. At start the same
	// conflict refuses the start instead.
	claimErr error
	// disabled holds the ways of receiving (receive, receive_http) the
	// adapter declares yet the channel's capability status reports off
	// by its own configuration (model.Disabled), such as polling for a
	// Telegram bot that takes its webhook: neither started nor mounted,
	// and no fault.
	disabled map[model.Capability]bool
}

// receives reports whether the channel receives by c: its adapter
// declares c and the channel's configuration does not switch it off.
func (in inbound) receives(c model.Capability) bool {
	return in.known && adapters.Declares(in.ad.Factory.Describe(), c) && !in.disabled[c]
}

// receivesAny reports whether the channel receives at all, persistently
// or over HTTP.
func (in inbound) receivesAny() bool {
	return in.receives(model.CapabilityReceive) || in.receives(model.CapabilityReceiveHTTP)
}

// claims reports whether the channel needs its provider account
// claimed: it receives and the account is named.
func (in inbound) claims() bool {
	return in.receivesAny() && in.accountErr == nil
}

// holds reports whether the channel holds its account claim.
func (in inbound) holds() bool { return in.claims() && in.claimErr == nil }

// unusable is why the channel cannot receive on its account, nil when
// it can.
func (in inbound) unusable() error {
	switch {
	case in.accountErr != nil:
		return fmt.Errorf("provider account: %w", in.accountErr)
	case in.claimErr != nil:
		return fmt.Errorf("provider account: %w", in.claimErr)
	}
	return nil
}

// inboundPlan is every channel of the profile, shared by the receivers
// and the webhook: a provider account is checked and claimed once,
// whichever service receives it.
type inboundPlan struct {
	channels []inbound
}

// planUser is a service running the plan's channels: receivers, the
// webhook. A reconcile stops what changed, then starts it again.
type planUser interface {
	// startChannels starts what the user runs for each channel.
	startChannels(r *running, chans []inbound)
	// stopChannels stops what the user runs for each channel and drops
	// its scopes, within ctx.
	stopChannels(ctx context.Context, chans []inbound)
}

// attach builds the plan on first use, as inbound does, starts u on
// every channel of it and registers u for later reconciles, all under
// the plan lock: no reconcile runs between u's start and its
// registration.
func (r *running) attach(ctx context.Context, u planUser) error {
	r.planMu.Lock()
	defer r.planMu.Unlock()
	p, err := r.inboundLocked(ctx)
	if err != nil {
		return err
	}
	u.startChannels(r, p.channels)
	r.users = append(r.users, u)
	return nil
}

// detach stops reconciles reaching u. It waits for a reconcile in
// progress.
func (r *running) detach(u planUser) {
	r.planMu.Lock()
	defer r.planMu.Unlock()
	for i, x := range r.users {
		if x == u {
			r.users = append(r.users[:i], r.users[i+1:]...)
			return
		}
	}
}

// inboundLocked lists the profile's channels on first use, names each
// receiving channel's provider account, refuses two channels on one
// account, and claims every account on this host for the run. The
// claims are held until the runtime closes, or a reconcile stops their
// channel. The outcome is kept: a second service gets the same plan,
// or the same refusal.
func (r *running) inboundLocked(ctx context.Context) (*inboundPlan, error) {
	if r.planDone {
		return r.plan, r.planErr
	}
	p, err := r.buildPlan(ctx)
	if err == nil {
		err = r.claimAccounts(p)
	}
	if err != nil {
		r.releaseClaimsLocked()
		r.planErr = err
	} else {
		r.plan = p
	}
	r.planDone = true
	return r.plan, r.planErr
}

// describe reads how each channel receives: its adapter in this build,
// the ways of receiving its configuration switches off and, for a
// receiving one, its provider account.
func (r *running) describe(ctx context.Context, chans []model.Channel) []inbound {
	out := make([]inbound, 0, len(chans))
	for _, ch := range chans {
		in := inbound{ch: ch}
		in.ad, in.known = r.adapters.Lookup(ch.Type)
		if in.known && adapters.Receives(in.ad.Factory.Describe()) {
			in.disabled = r.disabledReceiving(ctx, ch)
			if in.receivesAny() {
				in.account, in.accountErr = in.ad.AccountOf(ch)
			}
		}
		out = append(out, in)
	}
	return out
}

// capabilityReadTimeout bounds one channel's capability status read: an
// adapter that probes its provider's permissions calls out to it.
const capabilityReadTimeout = 10 * time.Second

// disabledReceiving reads ch's capability status, as uma reports it for
// the channel (Configuration.Capabilities: the adapter's declaration
// merged with what the channel's instance reports), and returns the ways
// of receiving reported unavailable with reason model.Disabled: off by
// the channel's own configuration. Any other unavailable reason is left
// to the receiver and the route, which meet the engine's refusal and
// report it on the channel's scope as a fault. A status that cannot be
// read switches nothing off: the channel is run by its adapter's
// declaration, and starting it reports what is wrong.
func (r *running) disabledReceiving(ctx context.Context, ch model.Channel) map[model.Capability]bool {
	c, err := r.engine.For(r.principal)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, capabilityReadTimeout)
	defer cancel()
	statuses, err := c.Configuration().Capabilities(ctx, model.Lookup[model.ChannelRef]{
		Call: model.Call{Correlation: correlation}, Ref: ch.Ref,
	})
	if err != nil {
		r.logInfo("runtime: channel capability status unread; following its adapter's declaration",
			"scope", scopeName(ch), "error", err)
		return nil
	}
	var off map[model.Capability]bool
	for _, st := range statuses {
		switch st.Name {
		case model.CapabilityReceive, model.CapabilityReceiveHTTP:
			if !st.Available && st.Reason == model.Disabled {
				if off == nil {
					off = map[model.Capability]bool{}
				}
				off[st.Name] = true
			}
		}
	}
	return off
}

func (r *running) buildPlan(ctx context.Context) (*inboundPlan, error) {
	chans, err := r.channels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	p := &inboundPlan{channels: r.describe(ctx, chans)}
	byAccount := map[string][]model.Channel{}
	for _, in := range p.channels {
		if in.claims() {
			byAccount[in.account] = append(byAccount[in.account], in.ch)
		}
	}
	var dups []string
	for _, chs := range byAccount {
		if len(chs) < 2 {
			continue
		}
		names := make([]string, 0, len(chs))
		for _, ch := range chs {
			names = append(names, channelName(ch))
		}
		sort.Strings(names)
		dups = append(dups, strings.Join(names, ", "))
	}
	if len(dups) > 0 {
		sort.Strings(dups)
		return nil, fmt.Errorf("channels %s receive the same provider account; "+
			"one account has one receiver: remove or reconfigure all but one", strings.Join(dups, "; "))
	}
	return p, nil
}

// holder names this daemon in a claim another daemon's refusal reads.
func (r *running) holder() string {
	return fmt.Sprintf("%s (pid %d)", r.deps.Owner, os.Getpid())
}

// claimAccounts takes every planned account on this host.
func (r *running) claimAccounts(p *inboundPlan) error {
	if r.deps.Accounts == nil {
		return nil
	}
	for _, in := range p.channels {
		if !in.claims() {
			continue
		}
		c, err := r.deps.Accounts.Claim(in.account, r.holder())
		if err != nil {
			var ce *claimedError
			if errors.As(err, &ce) {
				return fmt.Errorf("channel %s: its provider account is already %s; "+
					"one account has one receiver: stop that daemon or remove the channel from one profile",
					channelName(in.ch), ce.Error())
			}
			return fmt.Errorf("channel %s: %w", channelName(in.ch), err)
		}
		r.claims[in.account] = c
	}
	return nil
}

// planDiff is what a reconcile changes: the channels kept as they run,
// those stopped — removed, changed, or waiting for their account — and
// those started in their place or anew.
type planDiff struct {
	keep, stop, start []inbound
}

func (d planDiff) empty() bool { return len(d.stop) == 0 && len(d.start) == 0 }

// gone reports whether channel id is stopped and not started again.
func (d planDiff) gone(id model.ID) bool {
	for _, in := range d.start {
		if in.ch.Ref.ID == id {
			return false
		}
	}
	for _, in := range d.stop {
		if in.ch.Ref.ID == id {
			return true
		}
	}
	return false
}

// diffPlan compares the running channels cur with the stored ones next.
// A channel at the revision it runs at is kept, unless it waits for its
// account: that one is tried again. Any other is stopped, and started
// again when it is still stored.
func diffPlan(cur, next []inbound) planDiff {
	running := make(map[model.ID]inbound, len(cur))
	for _, in := range cur {
		running[in.ch.Ref.ID] = in
	}
	var d planDiff
	stored := make(map[model.ID]bool, len(next))
	for _, in := range next {
		stored[in.ch.Ref.ID] = true
		was, ok := running[in.ch.Ref.ID]
		switch {
		case ok && was.ch.Revision == in.ch.Revision && was.claimErr == nil:
			d.keep = append(d.keep, was)
		case ok:
			d.stop = append(d.stop, was)
			d.start = append(d.start, in)
		default:
			d.start = append(d.start, in)
		}
	}
	for _, in := range cur {
		if !stored[in.ch.Ref.ID] {
			d.stop = append(d.stop, in)
		}
	}
	return d
}

// assignAccounts gives each starting channel its provider account, in
// channel ID order: an account a kept channel, or an earlier starting
// one, receives is refused (claimErr), as is one claim refuses. One
// account has one receiver, and the channel already receiving it keeps
// it.
func assignAccounts(keep, start []inbound, claim func(inbound) error) {
	taken := map[string]model.Channel{}
	for _, in := range keep {
		if in.holds() {
			taken[in.account] = in.ch
		}
	}
	sort.SliceStable(start, func(i, j int) bool { return start[i].ch.Ref.ID < start[j].ch.Ref.ID })
	for i := range start {
		in := &start[i]
		if !in.claims() {
			continue
		}
		if o, ok := taken[in.account]; ok {
			in.claimErr = fmt.Errorf("channel %s already receives this provider account; "+
				"one account has one receiver: remove or reconfigure one of them", channelName(o))
			continue
		}
		if err := claim(*in); err != nil {
			in.claimErr = err
			continue
		}
		taken[in.account] = in.ch
	}
}

// reconcile brings the running receivers and webhook routes to the
// channels as stored: a channel added starts, one whose revision
// changed is stopped and started again, one no longer stored stops,
// and the others keep running untouched. Accounts are checked as at
// start, but a conflict fails the channel on its own scope instead of
// the service. Before any receiving service has started there is
// nothing to reconcile: the first one to start reads the channels.
func (r *running) reconcile(ctx context.Context) error {
	r.planMu.Lock()
	defer r.planMu.Unlock()
	if !r.planDone || r.plan == nil {
		return nil
	}
	chans, err := r.channels(ctx)
	if err != nil {
		return fmt.Errorf("list channels: %w", err)
	}
	d := diffPlan(r.plan.channels, r.describe(ctx, chans))
	if d.empty() {
		return nil
	}
	// Stop first, so an account a stopped channel held is free for the
	// channels starting, and no account is ever received twice.
	for _, u := range r.users {
		u.stopChannels(ctx, d.stop)
	}
	for _, in := range d.stop {
		if in.holds() {
			if c := r.claims[in.account]; c != nil {
				c.Release()
				delete(r.claims, in.account)
			}
		}
	}
	assignAccounts(d.keep, d.start, r.claimOne)
	for _, in := range d.start {
		if in.claimErr != nil {
			r.logError("runtime: channel refused", "scope", scopeName(in.ch), "error", in.claimErr)
		}
	}
	for _, u := range r.users {
		u.startChannels(r, d.start)
	}
	r.plan = &inboundPlan{channels: append(d.keep, d.start...)}
	r.logInfo("runtime: channels reconciled", "kept", len(d.keep), "stopped", len(d.stop), "started", len(d.start))
	return nil
}

// claimOne claims one account on this host for a channel added or
// changed while the daemon runs.
func (r *running) claimOne(in inbound) error {
	if r.deps.Accounts == nil {
		return nil
	}
	c, err := r.deps.Accounts.Claim(in.account, r.holder())
	if err != nil {
		var ce *claimedError
		if errors.As(err, &ce) {
			return fmt.Errorf("already %s; one account has one receiver: "+
				"stop that daemon or remove the channel from one profile", ce.Error())
		}
		return err
	}
	r.claims[in.account] = c
	return nil
}

// releaseClaims gives up every account claim of the run.
func (r *running) releaseClaims() {
	r.planMu.Lock()
	defer r.planMu.Unlock()
	r.releaseClaimsLocked()
}

func (r *running) releaseClaimsLocked() {
	for _, c := range r.claims {
		c.Release()
	}
	clear(r.claims)
}

// Reconcile applies the channels as stored to the running receivers
// and webhook routes, without a restart: a channel or inbox command
// calls it once its write commits. Only what changed is touched; see
// running.reconcile. Without a running runtime it does nothing. Errors
// are logged as well as returned: the write they follow stands.
func (h *Host) Reconcile(ctx context.Context) error {
	h.mu.Lock()
	r := h.run
	if r == nil {
		h.mu.Unlock()
		return nil
	}
	h.refs++
	h.mu.Unlock()
	defer func() { _ = h.release(context.WithoutCancel(ctx)) }()
	err := r.reconcile(ctx)
	if err != nil {
		r.logError("runtime: reconcile failed", "error", err)
	}
	return err
}
