// Package adapters is the set of uma adapters a heo binary runs: each
// uma adapter factory, plus what heo needs to own its receivers.
//
// The composition root builds a Registry by explicit registration;
// nothing registers itself at init. An empty registry is valid: the
// daemon then receives nothing and still serves, and a channel whose
// adapter this binary lacks is reported broken on its own.
//
// heo owns one receiver per provider account (two channels, or two
// profiles, never receive the same account). uma's receiver lease is
// per channel and cannot see two channels pointing at one account, so
// every adapter that receives — persistently or over HTTP — names the
// provider account a channel connects to through Account.
package adapters

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sync"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Adapter is one uma adapter factory and heo's view of its accounts.
type Adapter struct {
	// Factory is the uma adapter factory. Required.
	Factory adapter.Factory
	// Account names the provider account a channel of this adapter
	// connects to, such as a bot user, a mailbox or a workspace app:
	// two channels with the same account would receive the same
	// traffic. It reads the channel record and heo's own settings,
	// never a credential, and returns a stable, non-empty value.
	// Required when the adapter declares receive or receive_http.
	Account func(model.Channel) (string, error)
	// Routes returns handlers the webhook listener mounts beside a
	// channel's HTTP receiver, by path relative to the channel's route
	// ("/events" for <route>/events), such as a browser event stream.
	// Each handler sees paths relative to the route, like the
	// receiver. Optional; only for adapters that declare receive_http.
	Routes func(model.Channel) map[string]http.Handler
	// Serving, for an adapter whose persistent receiver serves on a
	// listener heo binds (the smtp.mail MX), carries the word that a
	// channel's listener is bound. The receivers service reports such a
	// channel starting, and holds its own readiness, until the listener
	// binds or the channel settles waiting or failed. Nil for an adapter
	// whose receiver binds nothing heo can see. Optional; only for
	// adapters that declare receive.
	Serving *Serving
	// ReportsLink is true for an adapter whose instances implement
	// adapter.ReceiverReporter: the Discord gateway, Slack Socket Mode,
	// Telegram polling and IMAP receivers. uma's receiver status then
	// shows no link only before the instance exists, which reads as not
	// connected yet, where for an adapter that reports none it shows
	// none for as long as the receiver runs and the receiver reads as
	// running. A link seen once marks the channel's adapter the same, so
	// a mistake here is a brief false ready at the start of a
	// connection, not a wrong state for good. Optional; only for
	// adapters that declare receive.
	ReportsLink bool
	// Staggered is true for an adapter whose receiver polls the
	// provider on a timer and renews provider-side registrations (a
	// watch, a subscription) from its start: the daemon spreads the
	// first connection of such receivers over
	// services.receivers.start_spread (uma's ReceiveStartSpread), so
	// their first polls do not all fall in one second. Optional; only for
	// adapters that declare receive.
	Staggered bool
	// CredentialPurposes are the purposes a channel's credential
	// reference resolves for, in the order the adapter uses them (uma
	// adapters declare them as package constants, not in the
	// descriptor). `channel check --remote` diagnoses the credential
	// behind each. Empty for an adapter that resolves none.
	CredentialPurposes []string
	// CredentialReceiving names, for each of CredentialPurposes that
	// only one way of receiving resolves, that way (receive or
	// receive_http): a request URL's signing secret, a socket's
	// app-level token. A channel whose capability status reports that
	// way off by its own configuration (model.Disabled) needs no such
	// credential, as the daemon then neither starts nor mounts it, and
	// `channel check --remote` does not diagnose it. A purpose not
	// named here is always needed. Optional.
	CredentialReceiving map[string]model.Capability
	// CredentialIf names, for each of CredentialPurposes the adapter
	// resolves only when the channel's own configuration asks for a
	// feature, the condition that makes it necessary: Microsoft Graph's
	// notification secret is needed only with a notification URL. The
	// adapter, finding it absent, runs without the feature and says
	// nothing, so heo reports it: a channel check finds an error, and
	// the channel's receiver reads waiting with Effect, not ready. A
	// channel the condition does not hold for is never asked for the
	// purpose: it is the operator's chosen mode, such as polling only.
	// Optional.
	CredentialIf map[string]CredentialCondition
	// CredentialSending names the CredentialPurposes only sending resolves:
	// a callback secret the adapter presents to a gateway. A channel whose
	// capability status reports send off by its own configuration
	// (model.Disabled), such as a relay with no callback, never presents
	// one, and `channel check --remote` does not diagnose it. A purpose not
	// named here is always needed. Optional.
	CredentialSending []string
	// CredentialOneOf lists groups of CredentialPurposes of which the
	// operator stores exactly one, such as a client secret or a client
	// certificate for the same app. `channel check --remote` diagnoses
	// each purpose of a group, reports one finding when none is stored and
	// a conflict when more than one is, and lists only the stored one.
	// Optional.
	CredentialOneOf [][]string
	// CredentialKinds names, for a purpose that accepts only some kinds of
	// credential, those kinds, preferred first. `channel check --remote`
	// diagnoses the purpose as the adapter would resolve it: a credential
	// of another kind found under the purpose (the reference's shared
	// entry, say) is unsupported, not present. A purpose not named here
	// is diagnosed as any kind. Optional.
	CredentialKinds map[string][]ports.CredentialKind
	// ProbeCredential, for an adapter whose host makes the credential the
	// adapter uses out of the one the operator stores (a token minted
	// from a client secret), tries that now: `channel check --remote`
	// runs it once the stored credentials diagnose as present, and
	// reports a problem as an error finding. It returns nil when the
	// credential works, and for a channel with nothing to try. It
	// reaches the provider; it never returns material. Optional.
	ProbeCredential func(ctx context.Context, ch model.Channel) *CredentialProblem
	// Extensions are the typed extension effects and reads the adapter
	// ships (a ticket update, say), registered with uma beside its
	// factory. Empty for an adapter that ships none.
	Extensions []adapter.ExtensionDefinition
	// CapabilityOff names, from a channel's own configuration, the
	// capabilities the adapter declares yet reports unavailable
	// there, each with the configuration key that switches it off
	// (github.issues without replies). Offline: it reads the channel
	// record only. Optional.
	CapabilityOff func(model.Channel) map[model.Capability]string
}

// CredentialProblem is a credential that is stored but does not work, or
// will not for long: what ProbeCredential found. Purpose names the stored
// credential to fix; Code is the provider's own error code, when it gave
// one. A Warning is a credential that works now, with something to say
// (a certificate close to its not_after): an error otherwise.
type CredentialProblem struct {
	Purpose string
	Code    string
	Message string
	Warning bool
}

// Serving relays an adapter's report that a channel's receiver listener
// is bound, or failed to bind, to whoever watches that channel. Safe for
// concurrent use; the zero value is not, use NewServing.
type Serving struct {
	mu      sync.Mutex
	watches map[model.ChannelRef]*servingWatch
}

type servingWatch struct{ f func(error) }

// NewServing returns a Serving nobody watches yet.
func NewServing() *Serving {
	return &Serving{watches: map[model.ChannelRef]*servingWatch{}}
}

// Report tells the watcher of ref that its receiver's listener is bound
// (err nil) or failed to bind (err). A report nobody watches is dropped.
// A nil Serving drops every report.
func (s *Serving) Report(ref model.ChannelRef, err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	w := s.watches[ref]
	s.mu.Unlock()
	if w != nil {
		w.f(err)
	}
}

// Watch calls f for every Report on ref until stop is called; a later
// Watch on ref replaces it.
func (s *Serving) Watch(ref model.ChannelRef, f func(error)) (stop func()) {
	w := &servingWatch{f: f}
	s.mu.Lock()
	s.watches[ref] = w
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if s.watches[ref] == w {
			delete(s.watches, ref)
		}
		s.mu.Unlock()
	}
}

// Receives reports whether d declares a receiver: persistent (receive)
// or over HTTP (receive_http).
func Receives(d adapter.Descriptor) bool {
	return Declares(d, model.CapabilityReceive) || Declares(d, model.CapabilityReceiveHTTP)
}

// Declares reports whether d declares capability c.
func Declares(d adapter.Descriptor, c model.Capability) bool {
	return slices.Contains(d.Capabilities, c)
}

// Registry holds the adapters a binary runs, in registration order.
type Registry struct {
	list   []Adapter
	byType map[model.TypeKey]int
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byType: map[model.TypeKey]int{}}
}

// Register adds a. A nil factory, a type registered twice, or a
// receiving adapter without Account is an error. uma validates the
// descriptor itself when the engine is composed.
func (r *Registry) Register(a Adapter) error {
	if a.Factory == nil {
		return errors.New("adapters: register nil factory")
	}
	d := a.Factory.Describe()
	if _, dup := r.byType[d.Type]; dup {
		return fmt.Errorf("adapters: type %q registered twice", d.Type)
	}
	if Receives(d) && a.Account == nil {
		return fmt.Errorf("adapters: type %q receives but names no provider account", d.Type)
	}
	if a.Routes != nil && !Declares(d, model.CapabilityReceiveHTTP) {
		return fmt.Errorf("adapters: type %q has routes but does not declare receive_http", d.Type)
	}
	if a.Serving != nil && !Declares(d, model.CapabilityReceive) {
		return fmt.Errorf("adapters: type %q reports serving but does not declare receive", d.Type)
	}
	if a.ReportsLink && !Declares(d, model.CapabilityReceive) {
		return fmt.Errorf("adapters: type %q reports its link but does not declare receive", d.Type)
	}
	for p, c := range a.CredentialReceiving {
		switch {
		case !slices.Contains(a.CredentialPurposes, p):
			return fmt.Errorf("adapters: type %q ties credential purpose %q to %s, but does not list the purpose", d.Type, p, c)
		case c != model.CapabilityReceive && c != model.CapabilityReceiveHTTP:
			return fmt.Errorf("adapters: type %q ties credential purpose %q to %s, which is not a way of receiving", d.Type, p, c)
		case !Declares(d, c):
			return fmt.Errorf("adapters: type %q ties credential purpose %q to %s, which it does not declare", d.Type, p, c)
		}
	}
	for _, p := range a.CredentialSending {
		switch {
		case !slices.Contains(a.CredentialPurposes, p):
			return fmt.Errorf("adapters: type %q ties credential purpose %q to send, but does not list the purpose", d.Type, p)
		case !Declares(d, model.CapabilitySend):
			return fmt.Errorf("adapters: type %q ties credential purpose %q to send, which it does not declare", d.Type, p)
		}
	}
	grouped := map[string]bool{}
	for _, g := range a.CredentialOneOf {
		if len(g) < 2 {
			return fmt.Errorf("adapters: type %q lists alternative credential purposes %v: want at least two", d.Type, g)
		}
		for _, p := range g {
			switch {
			case !slices.Contains(a.CredentialPurposes, p):
				return fmt.Errorf("adapters: type %q makes credential purpose %q an alternative, but does not list the purpose", d.Type, p)
			case grouped[p]:
				return fmt.Errorf("adapters: type %q puts credential purpose %q in more than one group of alternatives", d.Type, p)
			}
			grouped[p] = true
		}
	}
	for p, kinds := range a.CredentialKinds {
		switch {
		case !slices.Contains(a.CredentialPurposes, p):
			return fmt.Errorf("adapters: type %q names the kinds credential purpose %q accepts, but does not list the purpose", d.Type, p)
		case len(kinds) == 0:
			return fmt.Errorf("adapters: type %q names no kind credential purpose %q accepts", d.Type, p)
		}
	}
	r.byType[d.Type] = len(r.list)
	r.list = append(r.list, a)
	return nil
}

// Factories returns every registered factory, for uma's composition.
func (r *Registry) Factories() []adapter.Factory {
	out := make([]adapter.Factory, 0, len(r.list))
	for _, a := range r.list {
		out = append(out, a.Factory)
	}
	return out
}

// Extensions returns every registered adapter's extension
// definitions, in registration order, for uma's composition. uma
// registers one definition per name and version, so a definition two
// adapters ship identically (a stand-in that ships the effect of the
// adapter it replaces, in a test build) is returned once; one that
// differs is returned as it is, for uma to refuse where it is composed.
func (r *Registry) Extensions() []adapter.ExtensionDefinition {
	var out []adapter.ExtensionDefinition
	for _, a := range r.list {
		for _, d := range a.Extensions {
			if !slices.ContainsFunc(out, func(x adapter.ExtensionDefinition) bool { return reflect.DeepEqual(x, d) }) {
				out = append(out, d)
			}
		}
	}
	return out
}

// Lookup returns the adapter registered for t.
func (r *Registry) Lookup(t model.TypeKey) (Adapter, bool) {
	i, ok := r.byType[t]
	if !ok {
		return Adapter{}, false
	}
	return r.list[i], true
}

// Types lists the registered type keys, in registration order.
func (r *Registry) Types() []model.TypeKey {
	out := make([]model.TypeKey, 0, len(r.list))
	for _, a := range r.list {
		out = append(out, a.Factory.Describe().Type)
	}
	return out
}

// Len is the number of registered adapters.
func (r *Registry) Len() int { return len(r.list) }

// AccountOf returns the provider account ch connects to, qualified by
// its adapter type so two providers never collide.
func (a Adapter) AccountOf(ch model.Channel) (string, error) {
	if a.Account == nil {
		return "", errors.New("adapter names no provider account")
	}
	acct, err := a.Account(ch)
	if err != nil {
		return "", err
	}
	if acct == "" {
		return "", errors.New("adapter named an empty provider account")
	}
	return string(ch.Type) + "\x00" + acct, nil
}

// CredentialCondition makes a credential purpose necessary for the
// channels whose configuration asks for a feature of it.
type CredentialCondition struct {
	// Needed reports whether ch's configuration needs the credential.
	Needed func(ch model.Channel) bool
	// Effect is what the channel does without it, in an operator's
	// words: the feature that stays off.
	Effect string
}

// ConditionalPurposes returns the purposes of a's CredentialIf that ch's
// configuration needs, in the order of CredentialPurposes.
func (a Adapter) ConditionalPurposes(ch model.Channel) []string {
	var out []string
	for _, p := range a.CredentialPurposes {
		if c, ok := a.CredentialIf[p]; ok && c.Needed != nil && c.Needed(ch) {
			out = append(out, p)
		}
	}
	return out
}
