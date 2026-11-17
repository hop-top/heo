package desk_test

import (
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/trustcfg"
	"hop.top/uma/model"
)

// setup seeds a key's counters directly in uma and returns it with the
// inbox's channel.
type trustFix struct {
	w   *world
	ch  model.ChannelRef
	key string
	cfg trustcfg.Config
}

func newTrustFix(t *testing.T, verb, class string, imp model.ImportTrust) trustFix {
	t.Helper()
	w := newWorld(t)
	in, err := w.as(operator).FindInbox(t.Context(), "local")
	if err != nil {
		t.Fatal(err)
	}
	key, labels := desk.TrustKey("plugin:triage", "uid:1001", string(in.Ref.ID), verb, class)
	imp.Call, imp.Channel, imp.Key, imp.Labels = call, in.Channel, key, labels
	if _, err := w.client(service).Operations().ImportTrust(t.Context(), imp); err != nil {
		t.Fatal(err)
	}
	cfg := trustcfg.Defaults()
	cfg.Unattended = false
	return trustFix{w: w, ch: in.Channel, key: key, cfg: cfg}
}

func (f trustFix) set(t *testing.T, as string, req desk.TrustSetRequest) (desk.TrustSetResult, error) {
	t.Helper()
	req.Key = f.key
	actor := map[string]model.ActorID{"person": agent, "plugin": plugin}[as]
	return f.w.as(actor).SetTrust(t.Context(), f.w.client(service).Operations(), f.cfg, f.ch, req, f.w.clock.Now())
}

func (f trustFix) stored(t *testing.T) model.TrustEntry {
	t.Helper()
	e, err := f.w.client(service).Operations().Trust(t.Context(), model.TrustLookup{Call: call, Channel: f.ch, Key: f.key})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func n(v int64) *int64 { return &v }

func TestSetTrustMovesTheCountBothWays(t *testing.T) {
	f := newTrustFix(t, "state", "archive", model.ImportTrust{Clean: 5, Approvals: 9})
	for _, step := range []struct {
		name    string
		req     desk.TrustSetRequest
		clean   int64
		outcome string
		level   trustcfg.Level
	}{
		{"demote", desk.TrustSetRequest{Count: n(1)}, 1, desk.TrustSetCount, trustcfg.LevelAskInBatches},
		{"promote", desk.TrustSetRequest{Count: n(3)}, 3, desk.TrustSetCount, trustcfg.LevelAuto},
		{"same count", desk.TrustSetRequest{Count: n(3)}, 3, desk.TrustSetUnchanged, trustcfg.LevelAuto},
		{"reset", desk.TrustSetRequest{Reset: true}, 0, desk.TrustSetReset, trustcfg.LevelAskEach},
		{"reset again", desk.TrustSetRequest{Reset: true}, 0, desk.TrustSetUnchanged, trustcfg.LevelAskEach},
		{"count zero", desk.TrustSetRequest{Count: n(0)}, 0, desk.TrustSetUnchanged, trustcfg.LevelAskEach},
	} {
		res, err := f.set(t, "person", step.req)
		if err != nil || res.Outcome != step.outcome || res.After.Clean != step.clean || res.After.Level != step.level {
			t.Fatalf("%s = %+v, %v", step.name, res, err)
		}
		if e := f.stored(t); e.Clean != step.clean || e.Approvals != 9 || e.Corrections != 0 {
			t.Fatalf("%s: stored %+v, want clean %d with approvals 9 and no correction", step.name, e, step.clean)
		}
	}
}

// Neither an approval nor a correction: the totals and the last correction
// stay as they were, even on a key that was corrected, and a promotion
// after a correction still lands.
func TestSetTrustIsNeitherApprovalNorCorrection(t *testing.T) {
	when := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f := newTrustFix(t, "state", "archive", model.ImportTrust{Clean: 2, Approvals: 8, Corrections: 3, LastCorrectionAt: &when})
	res, err := f.set(t, "person", desk.TrustSetRequest{Count: n(6), Note: "earned elsewhere"})
	if err != nil || res.After.Clean != 6 || res.Note != "earned elsewhere" || res.Actor != string(agent) || res.At == "" {
		t.Fatalf("set = %+v, %v", res, err)
	}
	e := f.stored(t)
	if e.Clean != 6 || e.Approvals != 8 || e.Corrections != 3 || e.LastCorrectionAt == nil || !e.LastCorrectionAt.Equal(when) {
		t.Fatalf("stored = %+v", e)
	}
	// Lowering then raising again keeps the correction too.
	if _, err := f.set(t, "person", desk.TrustSetRequest{Reset: true}); err != nil {
		t.Fatal(err)
	}
	if e := f.stored(t); e.Clean != 0 || e.Corrections != 3 || e.LastCorrectionAt == nil || !e.LastCorrectionAt.Equal(when) {
		t.Fatalf("after reset = %+v", e)
	}
}

func TestSetTrustNeverAutoStopsAtBatches(t *testing.T) {
	f := newTrustFix(t, "state", "trash", model.ImportTrust{Clean: 2})
	f.cfg.NeverAuto = []string{"trash"}
	if _, err := f.set(t, "person", desk.TrustSetRequest{Count: n(4)}); err != nil {
		t.Fatalf("below the threshold refused: %v", err)
	}
	for _, c := range []int64{5, 99} {
		_, err := f.set(t, "person", desk.TrustSetRequest{Count: n(c)})
		if !model.IsCode(err, model.Validation) || !strings.Contains(err.Error(), "never_auto") {
			t.Fatalf("count %d: %v, want a never_auto refusal", c, err)
		}
	}
	if e := f.stored(t); e.Clean != 4 {
		t.Fatalf("a refused change wrote: %+v", e)
	}
	// Reset is always allowed.
	if res, err := f.set(t, "person", desk.TrustSetRequest{Reset: true}); err != nil || res.After.Clean != 0 || res.After.Level != trustcfg.LevelNeverAuto {
		t.Fatalf("reset = %+v, %v", res, err)
	}
}

func TestSetTrustRefusals(t *testing.T) {
	f := newTrustFix(t, "state", "archive", model.ImportTrust{Clean: 2})
	for name, c := range map[string]struct {
		as   string
		req  desk.TrustSetRequest
		code model.ErrorCode
	}{
		"plugin":       {"plugin", desk.TrustSetRequest{Count: n(3)}, model.Unauthorized},
		"plugin reset": {"plugin", desk.TrustSetRequest{Reset: true}, model.Unauthorized},
		"neither":      {"person", desk.TrustSetRequest{}, model.Validation},
		"both":         {"person", desk.TrustSetRequest{Count: n(1), Reset: true}, model.Validation},
		"negative":     {"person", desk.TrustSetRequest{Count: n(-1)}, model.Validation},
	} {
		if _, err := f.set(t, c.as, c.req); !model.IsCode(err, c.code) {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	if e := f.stored(t); e.Clean != 2 {
		t.Fatalf("a refused change wrote: %+v", e)
	}
	f.key = "plugin:triage/uid:1001/x/state:never-counted"
	if _, err := f.set(t, "person", desk.TrustSetRequest{Count: n(1)}); !model.IsCode(err, model.NotFound) {
		t.Fatalf("uncounted key: %v, want not found", err)
	}
	u := newTrustFix(t, "unsubscribe", "", model.ImportTrust{Clean: 2})
	if _, err := u.set(t, "person", desk.TrustSetRequest{Count: n(1)}); !model.IsCode(err, model.Validation) {
		t.Fatalf("unsubscribe: %v", err)
	}
}

func TestSetTrustDryRunWritesNothing(t *testing.T) {
	f := newTrustFix(t, "state", "archive", model.ImportTrust{Clean: 2})
	res, err := f.set(t, "person", desk.TrustSetRequest{Count: n(7), DryRun: true})
	if err != nil || !res.DryRun || res.Before.Clean != 2 || res.After.Clean != 7 || res.After.Level != trustcfg.LevelAuto {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if e := f.stored(t); e.Clean != 2 {
		t.Fatalf("a dry run wrote %+v", e)
	}
}

// A change moves nothing in held history: no held operation appears.
func TestSetTrustLeavesNoHeldOperation(t *testing.T) {
	f := newTrustFix(t, "state", "archive", model.ImportTrust{Clean: 2})
	if _, err := f.set(t, "person", desk.TrustSetRequest{Count: n(4)}); err != nil {
		t.Fatal(err)
	}
	p, err := f.w.as(agent).ListHeld(t.Context(), desk.OperationFilter{}, model.PageRequest{Limit: 50})
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("held = %d, %v", len(p.Items), err)
	}
}
