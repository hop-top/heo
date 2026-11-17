package channels

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/errmap"
	"hop.top/uma/model"
)

// Severities of a finding.
const (
	// SeverityError: the channel cannot work as configured.
	SeverityError = "error"
	// SeverityWarning: it works, with a limit the operator should know.
	SeverityWarning = "warning"
)

// Finding is one result of a channel check. Scope names what it is
// about: the channel, its configuration field, one of its inboxes, a
// capability or a credential purpose.
type Finding struct {
	Severity string `json:"severity" yaml:"severity" table:"SEVERITY"`
	Scope    string `json:"scope"    yaml:"scope"    table:"SCOPE"`
	Code     string `json:"code"     yaml:"code"     table:"CODE"`
	Message  string `json:"message"  yaml:"message"  table:"MESSAGE"`
}

// AdapterState is the channel's adapter in this build.
type AdapterState struct {
	Installed bool   `json:"installed"          yaml:"installed"`
	Maturity  string `json:"maturity,omitempty" yaml:"maturity,omitempty"`
}

// InboxState is one inbox of the channel, as the check saw it.
type InboxState struct {
	ID           model.ID `json:"id"                     yaml:"id"`
	Key          string   `json:"key,omitempty"          yaml:"key,omitempty"`
	Name         string   `json:"name"                   yaml:"name"`
	Destinations []string `json:"destinations,omitempty" yaml:"destinations,omitempty"`
	Disabled     bool     `json:"disabled"               yaml:"disabled"`
	DeliverNotes bool     `json:"deliver_notes"          yaml:"deliver_notes"`
}

// Capability is one capability the adapter declares, as the remote
// probe reports it now. Required marks one the channel's own
// configuration needs: receiving for an inbox that accepts inbound,
// send for replies, internal_note for an inbox delivering notes.
type Capability struct {
	Name      model.Capability `json:"name"             yaml:"name"`
	Available bool             `json:"available"        yaml:"available"`
	Reason    model.ErrorCode  `json:"reason,omitempty" yaml:"reason,omitempty"`
	Required  bool             `json:"required"         yaml:"required"`
}

// VerbLive is one provider-dependent verb as the remote probe reports it
// now: the state of the capability it needs, with the error code and
// exit status a command that hit that state would give. An undeclared
// capability is never reported by the adapter, so it reads unsupported.
type VerbLive struct {
	Verb       string           `json:"verb"       yaml:"verb"       table:"VERB"`
	Capability model.Capability `json:"capability" yaml:"capability" table:"CAPABILITY"`
	// State is available, or the reason it is not: unauthorized (the
	// provider account lacks the permission), unavailable, unsupported,
	// disabled (the adapter's own configuration switched it off).
	State string `json:"state" yaml:"state" table:"STATE"`
	// Code and Exit are the envelope code and exit status for the state;
	// empty and 0 when available.
	Code string `json:"code,omitempty" yaml:"code,omitempty" table:"CODE"`
	Exit int    `json:"exit"           yaml:"exit"           table:"EXIT"`
	// Limits are what the adapter reports of a conversation state verb
	// while it is available: whose view it changes (scope account or
	// shared), the flag kinds, the move shape and the roles it resolves.
	// Absent for every other verb, and for a report heo cannot read.
	Limits *model.StateLimits `json:"limits,omitempty" yaml:"limits,omitempty" table:"-"`
	// Recipients are what the adapter reports of the reply recipient
	// flags (reply-all, cc, bcc, and how many addresses a reply carries)
	// while it is available. Absent for every other verb, and for a
	// report heo cannot read.
	Recipients *model.RecipientLimits `json:"recipients,omitempty" yaml:"recipients,omitempty" table:"-"`
}

// ReceivingState is one way a channel receives, as the running daemon
// runs it: the adapter's persistent receiver (a gateway, a socket, a
// poll, a listener; way receive) or the channel's webhook route (way
// receive_http). What it reports is the daemon's own view: a receiver
// follows uma's status of its receive call: ready once the adapter's
// link is up (or, for an adapter that reports none, while the call runs
// and has not failed since it last accepted anything), waiting while
// the call reconnects.
type ReceivingState struct {
	Way model.Capability `json:"way" yaml:"way" table:"WAY"`
	// State is off (the channel's configuration switches the way off),
	// not_running (this daemon runs no service for it), starting, ready,
	// waiting (retrying; Reason says why) or failed (stopped for good).
	State  string `json:"state"            yaml:"state"            table:"STATE"`
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty" table:"REASON"`
	// Since is when the state was entered; zero for off and not_running.
	Since time.Time `json:"since,omitzero" yaml:"since,omitempty" table:"-"`
	// Path is the webhook route, for receive_http.
	Path string `json:"path,omitempty" yaml:"path,omitempty" table:"-"`
	// Owner and Until are the holder of the channel's receiver lease and
	// when it lapses unless renewed, for receive: one owner per channel.
	Owner string    `json:"owner,omitempty" yaml:"owner,omitempty" table:"-"`
	Until time.Time `json:"until,omitzero"  yaml:"until,omitempty" table:"-"`
	// Phase, Link, Failures, LastFailure and RetryAt are what uma reports
	// of the receive call, for receive, when the daemon answering runs
	// it: Phase is running (a connection is being opened or is open),
	// retrying (the last one failed) or ended; Link is the adapter's
	// own word on its provider link, connecting, connected or degraded, empty for
	// an adapter that reports none; Failures counts the consecutive
	// connections that accepted nothing, LastFailure is the error code
	// of the last, and RetryAt is when the next connection opens while
	// retrying. All empty when another daemon holds the lease (Owner
	// and Until say who) and when no call runs.
	Phase       string    `json:"phase,omitempty"        yaml:"phase,omitempty"        table:"-"`
	Link        string    `json:"link,omitempty"         yaml:"link,omitempty"         table:"-"`
	Failures    int       `json:"failures,omitempty"     yaml:"failures,omitempty"     table:"-"`
	LastFailure string    `json:"last_failure,omitempty" yaml:"last_failure,omitempty" table:"-"`
	RetryAt     time.Time `json:"retry_at,omitzero"      yaml:"retry_at,omitempty"     table:"-"`
	// Detail is, while Link is degraded (the receiver still receives,
	// by polling), the adapter's stable code for what is failing, such
	// as google.mail.push_denied.
	Detail string `json:"detail,omitempty" yaml:"detail,omitempty" table:"-"`
}

// Receiving states.
const (
	ReceivingOff        = "off"
	ReceivingNotRunning = "not_running"
	ReceivingStarting   = "starting"
	ReceivingReady      = "ready"
	ReceivingWaiting    = "waiting"
	ReceivingDegraded   = "degraded"
	ReceivingFailed     = "failed"
)

// Remote is what the authenticated probe found: the adapter's declared
// capabilities as the opened instance and, where the adapter probes
// them, the provider's permissions allow, intersected with what this
// version of uma executes; and the credential behind each purpose,
// diagnosed without any of its material.
type Remote struct {
	Capabilities []Capability `json:"capabilities" yaml:"capabilities"`
	// Verbs is each provider-dependent verb's live state, from the
	// capability report.
	Verbs []VerbLive `json:"verbs" yaml:"verbs"`
	// ReplyRecipients is the live state of the reply recipient flags
	// (`message reply` and `message reply-draft` with --reply-all, --cc
	// and --bcc), from the same report: the state of reply_recipients and,
	// while it is available, the limits the adapter reports.
	ReplyRecipients VerbLive                `json:"reply_recipients"      yaml:"reply_recipients"`
	Credentials     []credentials.Diagnosis `json:"credentials,omitempty" yaml:"credentials,omitempty"`
	// Receiving is each way the adapter receives, as the running daemon
	// runs it for this channel; empty when the check had no view of the
	// daemon.
	Receiving []ReceivingState `json:"receiving" yaml:"receiving"`
}

// ChannelCheck is one channel's check.
type ChannelCheck struct {
	ID            model.ID       `json:"id"                       yaml:"id"`
	Key           string         `json:"key,omitempty"            yaml:"key,omitempty"`
	Type          model.TypeKey  `json:"type"                     yaml:"type"`
	Revision      model.Revision `json:"revision"                yaml:"revision"`
	CredentialRef string         `json:"credential_ref,omitempty" yaml:"credential_ref,omitempty"`
	Adapter       AdapterState   `json:"adapter"                  yaml:"adapter"`
	// Account is the provider account the channel receives, as heo's
	// one-receiver-per-account rule keys it.
	Account  string       `json:"account,omitempty" yaml:"account,omitempty"`
	Inboxes  []InboxState `json:"inboxes"           yaml:"inboxes"`
	Remote   *Remote      `json:"remote,omitempty"  yaml:"remote,omitempty"`
	Findings []Finding    `json:"findings"          yaml:"findings"`
	// OK is true when no finding is an error.
	OK bool `json:"ok" yaml:"ok"`
}

// Diagnoser diagnoses the credential behind a reference for a purpose,
// never returning material: the profile's credential backend.
type Diagnoser interface {
	Check(ctx context.Context, req credentials.Request) credentials.Diagnosis
}

// GrantRefuser says whether the credential stored for a reference and
// purpose is an OAuth grant the profile's backend cannot serve: the
// profile's credential backend. It reads the credential store and
// reaches no provider.
type GrantRefuser interface {
	RefuseGrant(ctx context.Context, req credentials.Request) *credentials.GrantRefusal
}

// CheckOptions selects what a check does beyond the local checks.
type CheckOptions struct {
	// Grants refuses, in a local check, a stored OAuth grant the
	// profile's credential backend cannot serve. Nil checks none.
	Grants GrantRefuser
	// Remote runs the authenticated probe: it opens the channel's
	// adapter instance, which may reach the provider, and diagnoses its
	// credentials.
	Remote bool
	// Credentials diagnoses credentials: for a remote check, each of the
	// adapter's purposes; for any check, those the channel's
	// configuration makes necessary (adapters.Adapter.CredentialIf).
	Credentials Diagnoser
	// Receiving reads how the running daemon runs one way of receiving
	// (receive or receive_http) for a channel; its State is one of the
	// Receiving constants but off, which the check sets from the
	// channel's own configuration without asking. Nil reports nothing.
	Receiving func(ctx context.Context, ch model.Channel, way model.Capability) ReceivingState
}

// Check checks the channel ref, or with ref empty every channel the
// caller may read. The local checks read the data store only: the
// adapter is in this build, the configuration passes its validation,
// the provider account is derivable and no other channel receives it,
// and each inbox of the channel holds valid selection, distinct
// destinations, and note delivery its adapter declares. Nothing is
// written; nothing reaches a provider unless o.Remote.
func (s Service) Check(ctx context.Context, ref string, o CheckOptions) ([]ChannelCheck, error) {
	var targets []model.Channel
	all, err := s.allChannels(ctx)
	if err != nil {
		return nil, err
	}
	if ref == "" {
		if o.Remote {
			return nil, model.NewError(model.Validation, "channel: a remote check probes one channel; name it")
		}
		targets = all
	} else {
		ch, err := s.FindChannel(ctx, ref)
		if err != nil {
			return nil, err
		}
		targets = []model.Channel{ch}
	}
	inboxes, err := s.allInboxes(ctx)
	if err != nil {
		return nil, err
	}
	accounts := s.accounts(all)
	out := make([]ChannelCheck, 0, len(targets))
	for _, ch := range targets {
		c := s.checkLocal(ch, inboxes, accounts)
		if o.Grants != nil {
			s.checkGrants(ctx, ch, &c, o.Grants)
		}
		if o.Credentials != nil {
			s.checkConditional(ctx, ch, &c, o.Credentials)
		}
		if o.Remote {
			if err := s.checkRemote(ctx, ch, inboxes, &c, o); err != nil {
				return nil, err
			}
		}
		c.OK = !slices.ContainsFunc(c.Findings, func(f Finding) bool { return f.Severity == SeverityError })
		if c.Findings == nil {
			c.Findings = []Finding{}
		}
		if c.Inboxes == nil {
			c.Inboxes = []InboxState{}
		}
		out = append(out, c)
	}
	return out, nil
}

// accounts maps each receiving channel's provider account to the
// channels receiving it.
func (s Service) accounts(all []model.Channel) map[string][]model.Channel {
	out := map[string][]model.Channel{}
	for _, ch := range all {
		a, ok := s.Adapters.Lookup(ch.Type)
		if !ok || !adapters.Receives(a.Factory.Describe()) {
			continue
		}
		if acct, err := a.AccountOf(ch); err == nil {
			out[acct] = append(out[acct], ch)
		}
	}
	return out
}

func (s Service) checkLocal(ch model.Channel, inboxes []model.Inbox, accounts map[string][]model.Channel) ChannelCheck {
	c := ChannelCheck{ID: ch.Ref.ID, Key: ch.Key, Type: ch.Type, Revision: ch.Revision, CredentialRef: ch.CredentialRef}
	add := func(sev, scope, code, msg string) {
		c.Findings = append(c.Findings, Finding{Severity: sev, Scope: scope, Code: code, Message: msg})
	}
	a, ok := s.Adapters.Lookup(ch.Type)
	if !ok {
		add(SeverityError, "channel", "adapter_missing",
			fmt.Sprintf("adapter %s is not in this build: the channel cannot open", ch.Type))
	} else {
		d := a.Factory.Describe()
		c.Adapter = AdapterState{Installed: true, Maturity: d.Maturity}
		for _, i := range s.Engine.ValidateChannel(ch.Type, ch.Configuration) {
			add(SeverityError, "channel.configuration."+i.Field, i.Code, i.Message)
		}
		if adapters.Receives(d) {
			acct, err := a.AccountOf(ch)
			switch {
			case err != nil:
				add(SeverityWarning, "channel", "receives_nothing", "receives nothing: "+err.Error())
			default:
				c.Account = acct[len(ch.Type)+1:]
				for _, o := range accounts[acct] {
					if o.Ref != ch.Ref {
						add(SeverityError, "channel", "account_conflict", fmt.Sprintf(
							"channel %s receives the same provider account; one account has one receiver",
							Name(o.Key, o.Ref.ID)))
					}
				}
			}
		}
		if ch.CredentialRef == "" && len(a.CredentialPurposes) > 0 {
			add(SeverityWarning, "channel.credential_ref", "no_credential",
				"no credential reference, while the adapter resolves one for its provider calls")
		}
	}
	var mine []model.Inbox
	for _, in := range inboxes {
		if in.Channel == ch.Ref {
			mine = append(mine, in)
		}
	}
	if len(mine) == 0 {
		add(SeverityWarning, "channel", "no_inbox", "no inbox: nothing the channel receives is filed")
	}
	bound := map[string]model.Inbox{}
	for _, in := range mine {
		scope := "inbox:" + Name(in.Key, in.Ref.ID)
		c.Inboxes = append(c.Inboxes, InboxState{
			ID: in.Ref.ID, Key: in.Key, Name: in.Name, Destinations: slices.Clone(in.Destinations),
			Disabled: in.Disabled, DeliverNotes: in.DeliverNotes,
		})
		if err := in.Selection.Validate(); err != nil {
			add(SeverityError, scope, string(model.CodeOf(err)), safeMessage(err))
		}
		if len(in.Destinations) == 0 {
			add(SeverityWarning, scope, "no_destinations", "no destinations: the inbox receives nothing")
		}
		if in.Disabled {
			add(SeverityWarning, scope, "disabled", "disabled: inbound is rejected; queued sends still deliver")
		}
		if in.DeliverNotes && (!ok || !adapters.Declares(a.Factory.Describe(), model.CapabilityInternalNote)) {
			add(SeverityError, scope, string(model.Unsupported), "delivers notes, but the adapter does not declare internal_note")
		}
		for _, d := range in.Destinations {
			if o, dup := bound[d]; dup {
				add(SeverityError, scope, string(model.Conflict), fmt.Sprintf(
					"destination %q is also bound to inbox %s", d, Name(o.Key, o.Ref.ID)))
				continue
			}
			bound[d] = in
		}
	}
	return c
}

// checkGrants reports an OAuth grant stored for the channel's
// credential that the backend cannot serve, once per distinct refusal.
func (s Service) checkGrants(ctx context.Context, ch model.Channel, c *ChannelCheck, g GrantRefuser) {
	a, ok := s.Adapters.Lookup(ch.Type)
	if !ok || ch.CredentialRef == "" {
		return
	}
	var told []string
	for _, p := range a.CredentialPurposes {
		r := g.RefuseGrant(ctx, credentials.Request{Reference: ch.CredentialRef, Purpose: p, Accept: a.CredentialKinds[p]})
		if r == nil || slices.Contains(told, r.Message) {
			continue
		}
		told = append(told, r.Message)
		c.Findings = append(c.Findings, Finding{
			Severity: SeverityError, Scope: "credential:" + p, Code: "grant_not_writable", Message: r.Message,
		})
	}
}

// checkConditional reports, as an error, a credential the channel's own
// configuration needs (adapters.Adapter.CredentialIf) that is not
// stored: its adapter runs without the feature and says nothing. It
// reads the credential store and reaches no provider.
func (s Service) checkConditional(ctx context.Context, ch model.Channel, c *ChannelCheck, diag Diagnoser) {
	a, ok := s.Adapters.Lookup(ch.Type)
	if !ok || ch.CredentialRef == "" {
		return
	}
	for _, p := range a.ConditionalPurposes(ch) {
		d := diag.Check(ctx, credentials.Request{Reference: ch.CredentialRef, Purpose: p, Accept: a.CredentialKinds[p]})
		if d.Status == credentials.StatusOK || isStored(d.Status) {
			continue
		}
		msg := d.Message
		if d.SuggestedFix != "" {
			msg += "; " + d.SuggestedFix
		}
		if e := a.CredentialIf[p].Effect; e != "" {
			msg += "; " + e
		}
		c.Findings = append(c.Findings, Finding{
			Severity: SeverityError, Scope: "credential:" + p, Code: string(d.Status), Message: msg,
		})
	}
}

// checkRemote runs the authenticated probe as the caller: uma's
// capability report for the channel (refused unless the caller may read
// it) and, once that passed, the diagnosis of each credential purpose.
func (s Service) checkRemote(ctx context.Context, ch model.Channel, inboxes []model.Inbox, c *ChannelCheck, o CheckOptions) error {
	diag := o.Credentials
	add := func(sev, scope, code, msg string) {
		c.Findings = append(c.Findings, Finding{Severity: sev, Scope: scope, Code: code, Message: msg})
	}
	r := &Remote{Capabilities: []Capability{}, Verbs: []VerbLive{}, Receiving: []ReceivingState{}}
	c.Remote = r
	statuses, err := s.Client.Configuration().Capabilities(ctx, model.Lookup[model.ChannelRef]{Call: call(), Ref: ch.Ref})
	switch {
	case model.IsCode(err, model.Unauthorized), model.IsCode(err, model.NotFound):
		return err
	case err != nil:
		add(SeverityError, "capabilities", string(model.CodeOf(err)), "capability probe failed: "+safeMessage(err))
	}
	required := s.required(ch, inboxes)
	for _, st := range statuses {
		cp := Capability{Name: st.Name, Available: st.Available, Reason: st.Reason, Required: required[st.Name]}
		r.Capabilities = append(r.Capabilities, cp)
		if cp.Required && !cp.Available {
			add(SeverityError, "capability:"+string(st.Name), string(st.Reason),
				fmt.Sprintf("%s is unavailable (%s), and the channel's configuration needs it", st.Name, st.Reason))
		}
	}
	r.Verbs = VerbsLive(statuses)
	r.ReplyRecipients = RecipientsLive(statuses)
	off := switchedOff(statuses)
	s.checkReceiving(ctx, ch, required, off, r, add, o)
	a, ok := s.Adapters.Lookup(ch.Type)
	if !ok || ch.CredentialRef == "" || diag == nil {
		return nil
	}
	sendOff := sendSwitchedOff(statuses)
	stored := true
	diags := map[string]credentials.Diagnosis{}
	var asked []string
	for _, p := range a.CredentialPurposes {
		if _, cond := a.CredentialIf[p]; cond {
			// Needed only for some configurations, and found by the
			// check that runs for those (checkConditional).
			continue
		}
		way, onlyFor := a.CredentialReceiving[p]
		if onlyFor && off[way] {
			continue
		}
		if slices.Contains(a.CredentialSending, p) && sendOff {
			continue
		}
		diags[p] = diag.Check(ctx, credentials.Request{Reference: ch.CredentialRef, Purpose: p, Accept: a.CredentialKinds[p]})
		asked = append(asked, p)
	}
	// Of purposes that are alternatives one is stored: none is one finding
	// naming them all, more than one a conflict; a purpose not stored is
	// neither listed nor reported while another one is.
	handled := map[string]bool{}
	for _, g := range a.CredentialOneOf {
		members := slices.DeleteFunc(slices.Clone(g), func(p string) bool { _, ok := diags[p]; return !ok })
		if len(members) < 2 {
			continue
		}
		have := slices.DeleteFunc(slices.Clone(members), func(p string) bool { return !isStored(diags[p].Status) })
		switch {
		case len(have) > 1:
			stored = false
			add(SeverityError, "credential:"+have[0], string(model.Conflict), fmt.Sprintf(
				"more than one credential is stored for the channel (%s): keep exactly one", strings.Join(have, ", ")))
		case len(have) == 0 && !slices.ContainsFunc(members, func(p string) bool { return !isAbsent(diags[p].Status) }):
			stored = false
			for _, p := range members {
				handled[p] = true
			}
			add(SeverityError, "credential:"+members[0], string(credentials.StatusMissing), fmt.Sprintf(
				"no credential stored: store exactly one of %s", strings.Join(members, ", ")))
		case len(have) == 1:
			for _, p := range members {
				if isAbsent(diags[p].Status) {
					delete(diags, p)
				}
			}
		}
	}
	for _, p := range asked {
		d, ok := diags[p]
		if !ok {
			continue
		}
		r.Credentials = append(r.Credentials, d)
		if d.Status == credentials.StatusOK || handled[p] {
			continue
		}
		way, onlyFor := a.CredentialReceiving[p]
		sendOnly := slices.Contains(a.CredentialSending, p)
		// A credential the provider rejected is stored; when the host
		// can try it, the probe says why it fails, or that it works
		// now, and the rejection is not reported on its own.
		stored = stored && d.Status == credentials.StatusRejected
		if d.Status == credentials.StatusRejected && a.ProbeCredential != nil {
			continue
		}
		msg := d.Message
		if d.SuggestedFix != "" {
			msg += "; " + d.SuggestedFix
		}
		if onlyFor {
			msg += fmt.Sprintf("; needed for %s, which the channel's configuration leaves on", way)
		} else if sendOnly {
			msg += "; needed for send, which the channel's configuration leaves on"
		}
		add(SeverityError, "credential:"+p, string(d.Status), msg)
	}
	if a.ProbeCredential != nil && stored {
		if p := a.ProbeCredential(ctx, ch); p != nil {
			code := p.Code
			if code == "" {
				code = "credential_unusable"
			}
			sev := SeverityError
			if p.Warning {
				sev = SeverityWarning
			}
			add(sev, "credential:"+p.Purpose, code, p.Message)
		}
	}
	return nil
}

// isStored reports whether a diagnosis says a credential is stored, usable
// or not.
func isStored(s credentials.Status) bool {
	switch s {
	case credentials.StatusOK, credentials.StatusRejected, credentials.StatusExpired, credentials.StatusMalformed:
		return true
	}
	return false
}

// isAbsent reports whether a diagnosis says nothing usable by the adapter
// is stored under the purpose: nothing, or a credential of a kind it does
// not accept.
func isAbsent(s credentials.Status) bool {
	return s == credentials.StatusMissing || s == credentials.StatusUnsupported
}

// checkReceiving reports each way the adapter receives as the daemon
// runs it for ch, and what a failed or waiting receiver the channel's
// inboxes need makes of the check.
func (s Service) checkReceiving(ctx context.Context, ch model.Channel, required map[model.Capability]bool,
	off map[model.Capability]bool, r *Remote, add func(sev, scope, code, msg string), o CheckOptions,
) {
	if o.Receiving == nil {
		return
	}
	a, ok := s.Adapters.Lookup(ch.Type)
	if !ok {
		return
	}
	d := a.Factory.Describe()
	for _, way := range []model.Capability{model.CapabilityReceive, model.CapabilityReceiveHTTP} {
		if !adapters.Declares(d, way) {
			continue
		}
		st := ReceivingState{Way: way, State: ReceivingOff, Reason: "switched off by the channel's configuration"}
		if !off[way] {
			st = o.Receiving(ctx, ch, way)
			st.Way = way
		}
		r.Receiving = append(r.Receiving, st)
		if !required[way] {
			continue
		}
		scope := "receiving:" + string(way)
		switch st.State {
		case ReceivingFailed:
			add(SeverityError, scope, "receiver_failed", fmt.Sprintf("%s stopped: %s", way, st.Reason))
		case ReceivingWaiting:
			add(SeverityWarning, scope, "receiver_waiting", fmt.Sprintf("%s is waiting: %s", way, st.Reason))
		case ReceivingDegraded:
			add(SeverityWarning, scope, "receiver_degraded", fmt.Sprintf("%s is degraded: %s", way, st.Reason))
		}
	}
}

// switchedOff is the ways of receiving the statuses report off by the
// channel's own configuration (model.Disabled), as the daemon reads
// them: neither started nor mounted. Statuses that could not be read
// switch nothing off.
func switchedOff(statuses []model.CapabilityStatus) map[model.Capability]bool {
	out := map[model.Capability]bool{}
	for _, st := range statuses {
		if (st.Name == model.CapabilityReceive || st.Name == model.CapabilityReceiveHTTP) &&
			!st.Available && st.Reason == model.Disabled {
			out[st.Name] = true
		}
	}
	return out
}

// required lists the capabilities ch's inboxes need: receiving, for an
// enabled inbox with destinations; send, for replies in any inbox;
// internal_note, for an inbox delivering notes. Only declared ones.
func (s Service) required(ch model.Channel, inboxes []model.Inbox) map[model.Capability]bool {
	out := map[model.Capability]bool{}
	a, ok := s.Adapters.Lookup(ch.Type)
	if !ok {
		return out
	}
	d := a.Factory.Describe()
	need := func(c model.Capability) {
		if adapters.Declares(d, c) {
			out[c] = true
		}
	}
	for _, in := range inboxes {
		if in.Channel != ch.Ref {
			continue
		}
		need(model.CapabilitySend)
		if !in.Disabled && len(in.Destinations) > 0 {
			need(model.CapabilityReceive)
			need(model.CapabilityReceiveHTTP)
		}
		if in.DeliverNotes {
			need(model.CapabilityInternalNote)
		}
	}
	return out
}

// safeMessage is a uma error's safe message, or its text.
func safeMessage(err error) string {
	var me *model.Error
	if errors.As(err, &me) && me.SafeMessage != "" {
		return me.SafeMessage
	}
	return err.Error()
}

// VerbsLive maps the capability statuses a channel reported onto the
// provider-dependent verbs. A permission the provider account lacks is
// PROVIDER_PERMISSION, exit 5: refused like a caller denied, but a
// different remedy (the provider's grant, not heo's role).
func VerbsLive(statuses []model.CapabilityStatus) []VerbLive {
	out := []VerbLive{}
	for _, v := range adapters.ProviderVerbs() {
		out = append(out, verbLive(v.Name, v.Capability, statuses))
	}
	return out
}

// RecipientsLive maps the capability statuses a channel reported onto the
// reply recipient flags: the state of reply_recipients, and, while it is
// available, the limits it reports.
func RecipientsLive(statuses []model.CapabilityStatus) VerbLive {
	return verbLive(adapters.RecipientsVerb, model.CapabilityReplyRecipients, statuses)
}

func verbLive(name string, c model.Capability, statuses []model.CapabilityStatus) VerbLive {
	live := VerbLive{Verb: name, Capability: c, State: string(model.Unsupported)}
	if i := slices.IndexFunc(statuses, func(st model.CapabilityStatus) bool { return st.Name == c }); i >= 0 {
		switch st := statuses[i]; {
		case st.Available:
			live.State = "available"
			if c == model.CapabilityReplyRecipients {
				if l, err := model.ParseRecipientLimits(st.Limits); err == nil {
					live.Recipients = &l
				}
			} else if l, err := model.ParseStateLimits(st.Limits); err == nil {
				live.Limits = &l
			}
		case st.Reason == "":
			live.State = string(model.Unavailable)
		default:
			live.State = string(st.Reason)
		}
	}
	if live.State != "available" {
		e := errmap.Of(model.NewError(model.ErrorCode(live.State), live.State))
		live.Code, live.Exit = e.Code, e.ExitCode
		if live.State == string(model.Unauthorized) {
			live.Code = errmap.CodeProviderPermission
		}
	}
	return live
}

// sendSwitchedOff reports whether the statuses say send is off by the
// channel's own configuration (model.Disabled): nothing presents a
// credential only sending uses. Statuses that could not be read switch
// nothing off.
func sendSwitchedOff(statuses []model.CapabilityStatus) bool {
	return slices.ContainsFunc(statuses, func(st model.CapabilityStatus) bool {
		return st.Name == model.CapabilitySend && !st.Available && st.Reason == model.Disabled
	})
}
