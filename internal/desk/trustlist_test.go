package desk_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/trustcfg"
	"hop.top/uma/model"
)

type fakeTrusts struct {
	entries []model.TrustEntry
	got     model.TrustFilter
}

func (f *fakeTrusts) Trusts(_ context.Context, req model.TrustFilter) (model.Page[model.TrustEntry], error) {
	f.got = req
	return model.Page[model.TrustEntry]{Items: f.entries, Next: "k", Complete: false}, nil
}

func entryOf(clean, corrections int64, verb, class string) model.TrustEntry {
	key, labels := desk.TrustKey("plugin:triage", "uid:1001", "support", verb, class)
	return model.TrustEntry{Key: key, Labels: labels, Clean: clean, Corrections: corrections}
}

func TestListTrustDecidesEachKey(t *testing.T) {
	cfg := trustcfg.Defaults()
	cfg.Unattended = false
	cfg.Seed = []string{"archive"}
	when := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	corrected := entryOf(0, 1, "state", "archive")
	corrected.LastCorrectionAt = &when
	rd := &fakeTrusts{entries: []model.TrustEntry{
		entryOf(3, 0, "state", "read"),    // graduated
		entryOf(2, 0, "state", "trash"),   // training
		entryOf(0, 0, "state", "archive"), // seeded: automatic with no history
		corrected,                         // seeded, but corrected: held again
		entryOf(8, 0, "reply", "send"),    // graduated send
	}}
	ch := model.ChannelRef{Tenant: "t", ID: "ch"}
	p, err := desk.ListTrust(t.Context(), rd, cfg, ch, map[string]string{"verb": "state"}, model.PageRequest{Limit: 5}, when)
	if err != nil {
		t.Fatal(err)
	}
	if rd.got.Channel != ch || rd.got.Labels["verb"] != "state" || rd.got.Page.Limit != 5 || p.Next != "k" || p.Complete {
		t.Fatalf("request %+v page %+v", rd.got, p)
	}
	want := []bool{true, false, true, false, true}
	for i, w := range want {
		if p.Items[i].Automatic != w {
			t.Fatalf("row %d (%s %s) automatic = %v, want %v: %q", i, p.Items[i].Verb, p.Items[i].Class, p.Items[i].Automatic, w, p.Items[i].Reason)
		}
	}
	r := p.Items[3]
	if r.Proposer != "plugin:triage" || r.OnBehalfOf != "uid:1001" || r.Channel != "support" || r.Corrections != 1 || r.LastCorrectionAt == nil {
		t.Fatalf("parts = %+v", r)
	}
	if p.Items[1].Reason == "" || p.Items[0].Reason != "" {
		t.Fatalf("reasons: held %q automatic %q", p.Items[1].Reason, p.Items[0].Reason)
	}
}

func TestListTrustShowsUnsubscribeAlwaysHeld(t *testing.T) {
	cfg := trustcfg.Defaults()
	cfg.Unattended = false
	p, _ := desk.ListTrust(t.Context(), &fakeTrusts{entries: []model.TrustEntry{entryOf(500, 0, "unsubscribe", "")}},
		cfg, model.ChannelRef{}, nil, model.PageRequest{}, time.Now())
	if p.Items[0].Automatic || !strings.Contains(p.Items[0].Reason, "always held") {
		t.Fatalf("row = %+v", p.Items[0])
	}
}

func TestListTrustHonoursApprovalAlways(t *testing.T) {
	cfg := trustcfg.Defaults()
	cfg.ApprovalAlways = []string{"support"}
	p, _ := desk.ListTrust(t.Context(), &fakeTrusts{entries: []model.TrustEntry{entryOf(99, 0, "state", "read")}},
		cfg, model.ChannelRef{}, nil, model.PageRequest{}, time.Now())
	if p.Items[0].Automatic {
		t.Fatalf("approval-always mailbox listed automatic: %+v", p.Items[0])
	}
}

func TestListTrustShowsNeverAutoReason(t *testing.T) {
	cfg := trustcfg.Defaults()
	cfg.Unattended = false
	cfg.NeverAuto = []string{"trash"}
	p, _ := desk.ListTrust(t.Context(), &fakeTrusts{entries: []model.TrustEntry{entryOf(99, 0, "state", "trash"), entryOf(99, 0, "state", "spam")}},
		cfg, model.ChannelRef{}, nil, model.PageRequest{}, time.Now())
	if p.Items[0].Automatic || !strings.HasPrefix(p.Items[0].Reason, trustcfg.NeverAutoReasonPrefix) || p.Items[0].Clean != 99 {
		t.Fatalf("trash row = %+v", p.Items[0])
	}
	if !p.Items[1].Automatic {
		t.Fatalf("spam row = %+v, want automatic: it is not under never_auto here", p.Items[1])
	}
}

func TestListTrustLevelsAndThreshold(t *testing.T) {
	cfg := trustcfg.Defaults()
	cfg.Unattended = false
	cfg.NeverAuto = []string{"trash"}
	p, _ := desk.ListTrust(t.Context(), &fakeTrusts{entries: []model.TrustEntry{
		entryOf(0, 0, "state", "archive"), entryOf(2, 0, "state", "archive"), entryOf(3, 0, "state", "archive"),
		entryOf(99, 0, "state", "trash"), entryOf(99, 0, "unsubscribe", ""),
	}}, cfg, model.ChannelRef{}, nil, model.PageRequest{}, time.Now())
	want := []struct {
		level trustcfg.Level
		label string
		thr   int
	}{{trustcfg.LevelAskEach, "Ask each", 3}, {trustcfg.LevelAskInBatches, "Ask in batches", 3}, {trustcfg.LevelAuto, "Auto", 3},
		{trustcfg.LevelNeverAuto, "Never auto", 5}, {trustcfg.LevelNeverAuto, "Never auto", -1}}
	for i, w := range want {
		r := p.Items[i]
		if r.Level != w.level || r.LevelLabel != w.label {
			t.Errorf("row %d level = %s %q, want %s %q", i, r.Level, r.LevelLabel, w.level, w.label)
		}
		if (w.thr < 0) != (r.Threshold == nil) || w.thr >= 0 && *r.Threshold != w.thr {
			t.Errorf("row %d threshold = %v, want %d", i, r.Threshold, w.thr)
		}
	}
}
