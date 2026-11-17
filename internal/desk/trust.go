package desk

import (
	"context"
	"time"

	"hop.top/heo/internal/trustcfg"
	"hop.top/uma/model"
)

// TrustPolicy is the trust ladder as a hold policy: it replaces HoldAll
// when configured. uma counts the decisions under each write's trust
// key; the policy only reads the count and holds until the class has
// earned its threshold. It never writes a counter.
type TrustPolicy struct {
	Config trustcfg.Config
	// Channel resolves the inbox a write touches to the channel uma
	// scopes its counters to.
	Channel func(ctx context.Context, inbox string) (model.ChannelRef, error)
	// Trust reads the counters of one key.
	Trust func(ctx context.Context, ch model.ChannelRef, key string) (model.TrustEntry, error)
	// Now defaults to time.Now.
	Now func() time.Time
}

// Decide holds unless the ladder lets the write through. A counter that
// cannot be read holds: trust is never assumed.
func (p TrustPolicy) Decide(ctx context.Context, q HoldQuery) (HoldDecision, error) {
	in := trustcfg.Input{
		OnBehalfOf: string(q.OnBehalfOf), Channel: q.Channel, Verb: q.Verb, Class: q.Class,
		Facts: trustcfg.Facts(q.Facts), Now: time.Now(),
	}
	if p.Now != nil {
		in.Now = p.Now()
	}
	if q.Channel != "" && p.Channel != nil && p.Trust != nil {
		key, _ := TrustKey(q.Proposer, q.OnBehalfOf, q.Channel, q.Verb, q.Class)
		ch, err := p.Channel(ctx, q.Channel)
		if err != nil {
			return HoldDecision{Hold: true, Reason: "trust unreadable: " + err.Error()}, nil
		}
		e, err := p.Trust(ctx, ch, key)
		switch {
		case err == nil:
			in.Counts = trustcfg.Counts{Found: true, Clean: e.Clean, Corrections: e.Corrections}
		case model.CodeOf(err) != model.NotFound:
			return HoldDecision{Hold: true, Reason: "trust unreadable: " + err.Error()}, nil
		}
	}
	d := p.Config.Decide(in)
	return HoldDecision{Hold: d.Hold, Reason: d.Reason}, nil
}
