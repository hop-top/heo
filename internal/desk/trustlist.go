package desk

import (
	"context"
	"time"

	"hop.top/heo/internal/trustcfg"
	"hop.top/uma/model"
)

// TrustReader is the part of uma's operations a trust listing reads.
type TrustReader interface {
	Trusts(ctx context.Context, req model.TrustFilter) (model.Page[model.TrustEntry], error)
}

// TrustRow is one trust key as uma counts it, with the parts of the key
// (TrustKey's labels) and whether the ladder runs it automatically
// under the configuration. Reason says why a key is held.
type TrustRow struct {
	Key              string            `json:"key"                          yaml:"key"`
	Proposer         string            `json:"proposer,omitempty"           yaml:"proposer,omitempty"`
	OnBehalfOf       string            `json:"on_behalf_of,omitempty"       yaml:"on_behalf_of,omitempty"`
	Channel          string            `json:"channel,omitempty"            yaml:"channel,omitempty"`
	Verb             string            `json:"verb,omitempty"               yaml:"verb,omitempty"`
	Class            string            `json:"class,omitempty"              yaml:"class,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"             yaml:"labels,omitempty"`
	Clean            int64             `json:"clean"                        yaml:"clean"`
	Approvals        int64             `json:"approvals"                    yaml:"approvals"`
	Corrections      int64             `json:"corrections"                  yaml:"corrections"`
	LastCorrectionAt *time.Time        `json:"last_correction_at,omitempty" yaml:"last_correction_at,omitempty"`
	// Threshold is the clean approvals the class needs before it is
	// automatic under the configuration; absent for unsubscribe, which
	// has none.
	Threshold *int `json:"threshold,omitempty"          yaml:"threshold,omitempty"`
	// Level is where the key stands on the ladder: ask_each (no clean
	// approval), ask_in_batches (one up to the threshold), auto (at or
	// above it) or never_auto (held whatever the count). It is derived
	// here, from the count and the configuration, never by a client.
	Level      trustcfg.Level `json:"level"                        yaml:"level"`
	LevelLabel string         `json:"level_label"                  yaml:"level_label"`
	Automatic  bool           `json:"automatic"                    yaml:"automatic"`
	Reason     string         `json:"reason,omitempty"             yaml:"reason,omitempty"`
}

// TrustRowOf reads e's key parts from its labels and decides it under
// cfg, as the hold point would for a write under that key.
func TrustRowOf(cfg trustcfg.Config, e model.TrustEntry, now time.Time) TrustRow {
	l := e.Labels
	r := TrustRow{
		Key: e.Key, Proposer: l["proposer"], OnBehalfOf: l["on_behalf_of"], Channel: l["channel"],
		Verb: l["verb"], Class: l["class"], Labels: l, Clean: e.Clean, Approvals: e.Approvals,
		Corrections: e.Corrections, LastCorrectionAt: e.LastCorrectionAt,
	}
	d := cfg.Decide(trustcfg.Input{OnBehalfOf: r.OnBehalfOf, Channel: r.Channel, Verb: r.Verb, Class: r.Class,
		Counts: trustcfg.Counts{Found: true, Clean: e.Clean, Corrections: e.Corrections}, Now: now})
	r.Automatic, r.Reason = !d.Hold, d.Reason
	in := trustcfg.Input{OnBehalfOf: r.OnBehalfOf, Channel: r.Channel, Verb: r.Verb, Class: r.Class,
		Counts: trustcfg.Counts{Found: true, Clean: e.Clean, Corrections: e.Corrections}, Now: now}
	r.Threshold = cfg.ThresholdOf(r.Channel, r.Verb, r.Class)
	r.Level = cfg.Level(in)
	r.LevelLabel = r.Level.Label()
	return r
}

// TrustPage is one page of trust rows; Next continues the listing.
type TrustPage struct {
	Items    []TrustRow
	Next     model.Cursor
	Complete bool
}

// ListTrust reads one page of the counters of channel ch, narrowed to
// the entries whose labels include every pair of labels, and decides
// each under cfg. now is the clock the supervised week reads.
func ListTrust(ctx context.Context, rd TrustReader, cfg trustcfg.Config, ch model.ChannelRef,
	labels map[string]string, page model.PageRequest, now time.Time,
) (TrustPage, error) {
	p, err := rd.Trusts(ctx, model.TrustFilter{Call: call(), Channel: ch, Labels: labels, Page: page})
	if err != nil {
		return TrustPage{}, err
	}
	out := TrustPage{Items: make([]TrustRow, 0, len(p.Items)), Next: p.Next, Complete: p.Complete}
	for _, e := range p.Items {
		out.Items = append(out.Items, TrustRowOf(cfg, e, now))
	}
	return out, nil
}
