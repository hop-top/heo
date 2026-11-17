package desk_test

import (
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/trustcfg"
	"hop.top/uma/model"
)

func i64(n int64) *int64 { return &n }

func TestParseTrustImportIsStrict(t *testing.T) {
	ok := `{"version":1,"entries":[{"channel":"c","proposer":"plugin:triage","verb":"state","class":"read","clean":3}]}`
	f, err := desk.ParseTrustImport([]byte(ok))
	if err != nil || len(f.Entries) != 1 || *f.Entries[0].Clean != 3 {
		t.Fatalf("valid file: %+v, %v", f, err)
	}
	for name, in := range map[string]string{
		"unknown top-level field": `{"version":1,"entries":[{"channel":"c","proposer":"p","verb":"v","clean":1}],"extra":1}`,
		"unknown entry field":     `{"version":1,"entries":[{"channel":"c","proposer":"p","verb":"v","clean":1,"subject":"hi"}]}`,
		"version 2":               `{"version":2,"entries":[{"channel":"c"}]}`,
		"no version":              `{"entries":[{"channel":"c"}]}`,
		"no entries":              `{"version":1,"entries":[]}`,
		"second value":            ok + ` {}`,
		"not json":                `version: 1`,
		"bad time":                `{"version":1,"entries":[{"channel":"c","clean":1,"last_correction_at":"yesterday"}]}`,
	} {
		if _, err := desk.ParseTrustImport([]byte(in)); !model.IsCode(err, model.Validation) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
	}
}

func importOpts(w *world, cfg trustcfg.Config, dry bool) desk.TrustImportOptions {
	return desk.TrustImportOptions{DryRun: dry, Config: cfg, Now: w.clock.Now(), Read: w.client(service).Operations()}
}

func entry(channel, verb, class string, clean int64) desk.TrustImportEntry {
	return desk.TrustImportEntry{Channel: channel, Proposer: "plugin:triage", OnBehalfOf: "uid:1001", Verb: verb, Class: class, Clean: i64(clean)}
}

func storedTrust(t *testing.T, w *world, key string) (model.TrustEntry, bool) {
	t.Helper()
	in, err := w.as(operator).FindInbox(t.Context(), "local")
	if err != nil {
		t.Fatal(err)
	}
	e, err := w.client(service).Operations().Trust(t.Context(), model.TrustLookup{Call: call, Channel: in.Channel, Key: key})
	if model.IsCode(err, model.NotFound) {
		return model.TrustEntry{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return e, true
}

func TestImportTrustBuildsKeysAndIsIdempotent(t *testing.T) {
	w := newWorld(t)
	cfg := trustcfg.Defaults()
	cfg.Unattended = false
	in, _ := w.as(operator).FindInbox(t.Context(), "local")
	f := desk.TrustImportFile{Version: 1, Entries: []desk.TrustImportEntry{
		entry("local", "state", "read", 3),            // at threshold: automatic
		entry(string(in.Ref.ID), "state", "trash", 2), // by ID; under threshold
	}}
	res, err := w.as(operator).ImportTrust(t.Context(), f, importOpts(w, cfg, false))
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 2 || res.Unchanged != 0 || res.Refused != 0 || !res.OK() || res.Automatic != 1 {
		t.Fatalf("first import = %+v", res)
	}
	key, _ := desk.TrustKey("plugin:triage", "uid:1001", string(in.Ref.ID), "state", "read")
	if res.Entries[0].Key != key || !res.Entries[0].Automatic || !res.Entries[0].BecomesAutomatic || res.Entries[1].Automatic {
		t.Fatalf("rows = %+v", res.Entries)
	}
	if e, ok := storedTrust(t, w, key); !ok || e.Clean != 3 || e.Approvals != 3 || e.Labels["verb"] != "state" {
		t.Fatalf("stored = %+v, %v", e, ok)
	}

	again, err := w.as(operator).ImportTrust(t.Context(), f, importOpts(w, cfg, false))
	if err != nil {
		t.Fatal(err)
	}
	if again.Imported != 0 || again.Unchanged != 2 || again.Automatic != 0 {
		t.Fatalf("re-import = %+v, want both unchanged", again)
	}
	before, _ := storedTrust(t, w, key)
	f.Entries[0].Clean = i64(1) // a lower count never lowers the stored one
	if _, err := w.as(operator).ImportTrust(t.Context(), f, importOpts(w, cfg, false)); err != nil {
		t.Fatal(err)
	}
	if after, _ := storedTrust(t, w, key); after.Clean != 3 || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("a lower import changed %+v to %+v", before, after)
	}
}

func TestImportTrustDryRunWritesNothing(t *testing.T) {
	w := newWorld(t)
	cfg := trustcfg.Defaults()
	cfg.Unattended = false
	in, _ := w.as(operator).FindInbox(t.Context(), "local")
	f := desk.TrustImportFile{Version: 1, Entries: []desk.TrustImportEntry{entry("local", "state", "read", 3), entry("local", "state", "trash", 1)}}
	res, err := w.as(operator).ImportTrust(t.Context(), f, importOpts(w, cfg, true))
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.Imported != 2 || res.Automatic != 1 || !res.Entries[0].BecomesAutomatic || res.Entries[1].Automatic {
		t.Fatalf("dry run = %+v", res)
	}
	key, _ := desk.TrustKey("plugin:triage", "uid:1001", string(in.Ref.ID), "state", "read")
	if e, ok := storedTrust(t, w, key); ok {
		t.Fatalf("a dry run wrote %+v", e)
	}
}

func TestImportTrustRefusesTheWholeFile(t *testing.T) {
	w := newWorld(t)
	cfg := trustcfg.Defaults()
	in, _ := w.as(operator).FindInbox(t.Context(), "local")
	bad := map[string]desk.TrustImportEntry{
		"unknown channel": entry("nope", "state", "read", 1),
		"unknown verb":    entry("local", "delete-everything", "", 1),
		"label-create":    entry("local", "label-create", "Finance", 1),
		"unsubscribe":     entry("local", "unsubscribe", "", 50),
		"reply class":     entry("local", "reply", "draft", 1),
		"start class":     entry("local", "start", "x", 1),
		"note class":      entry("local", "note", "nowhere", 1),
		"state no class":  entry("local", "state", "", 1),
		"not a plugin":    {Channel: "local", Proposer: "uid:1", Verb: "state", Class: "read", Clean: i64(1)},
		"no proposer":     {Channel: "local", Verb: "state", Class: "read", Clean: i64(1)},
		"no clean":        {Channel: "local", Proposer: "plugin:triage", Verb: "state", Class: "read"},
		"negative clean":  entry("local", "state", "read", -1),
	}
	for name, e := range bad {
		f := desk.TrustImportFile{Version: 1, Entries: []desk.TrustImportEntry{entry("local", "state", "read", 3), e}}
		res, err := w.as(operator).ImportTrust(t.Context(), f, importOpts(w, cfg, false))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Refused != 1 || res.Imported != 0 || res.OK() || res.Entries[0].Status != desk.TrustNotWritten ||
			res.Entries[1].Status != desk.TrustRefused || !strings.HasPrefix(res.Entries[1].Reason, "entry 1: ") {
			t.Errorf("%s: %+v", name, res)
		}
	}
	key, _ := desk.TrustKey("plugin:triage", "uid:1001", string(in.Ref.ID), "state", "read")
	if e, ok := storedTrust(t, w, key); ok {
		t.Fatalf("a refused file wrote %+v", e)
	}
	dup := desk.TrustImportFile{Version: 1, Entries: []desk.TrustImportEntry{entry("local", "state", "read", 3), entry(string(in.Ref.ID), "state", "read", 4)}}
	if res, _ := w.as(operator).ImportTrust(t.Context(), dup, importOpts(w, cfg, false)); res.Refused != 1 || !strings.Contains(res.Entries[1].Reason, "same key as entry 0") {
		t.Fatalf("duplicate key: %+v", res)
	}
}

func TestImportTrustNeedsTheRightToWrite(t *testing.T) {
	w := newWorld(t)
	f := desk.TrustImportFile{Version: 1, Entries: []desk.TrustImportEntry{entry("local", "state", "read", 3)}}
	res, err := w.as(agent).ImportTrust(t.Context(), f, importOpts(w, trustcfg.Defaults(), false))
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 || res.Entries[0].Status != desk.TrustFailed || res.Imported != 0 {
		t.Fatalf("desk import = %+v, want a failed entry", res)
	}
}

// The projection a dry run shows is uma's own merge: held to the real
// store over the cases uma's import documents.
func TestMergeTrustMatchesUma(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	in, _ := w.as(operator).FindInbox(ctx, "local")
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	future := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	imp := func(clean int64, last *time.Time) model.ImportTrust {
		return model.ImportTrust{Call: call, Channel: in.Channel, Key: "merge/key", Labels: map[string]string{"verb": "state"}, Clean: clean, LastCorrectionAt: last}
	}
	ops := w.client(operator).Operations()
	var cur *model.TrustEntry
	steps := []model.ImportTrust{imp(5, &past), imp(5, &past), imp(2, nil), imp(9, nil), imp(50, &past), imp(70, &future), imp(1, nil), imp(0, &past)}
	for i, step := range steps {
		want := desk.MergeTrust(cur, step)
		got, err := ops.ImportTrust(ctx, step)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got.Clean != want.Clean || got.Approvals != want.Approvals || got.Corrections != want.Corrections ||
			(got.LastCorrectionAt == nil) != (want.LastCorrectionAt == nil) ||
			got.LastCorrectionAt != nil && !got.LastCorrectionAt.Equal(*want.LastCorrectionAt) {
			t.Fatalf("step %d: uma %+v, projection %+v", i, got, want)
		}
		cur = &got
	}
}

// TestImportTrustTakesTheProviderStateClasses: the classes the provider
// state verbs are held under (state:<class> and move:<class>) import like
// any other held verb's.
func TestImportTrustTakesTheProviderStateClasses(t *testing.T) {
	w := newWorld(t)
	f := desk.TrustImportFile{Version: 1, Entries: []desk.TrustImportEntry{
		entry("local", "state", "archive", 3), entry("local", "state", "spam", 5), entry("local", "state", "unstar", 3),
		entry("local", "move", "inbox", 3), entry("local", "move", "+Label_12,-INBOX", 3),
	}}
	res, err := w.as(operator).ImportTrust(t.Context(), f, importOpts(w, trustcfg.Defaults(), true))
	if err != nil || !res.OK() || res.Refused != 0 {
		t.Fatalf("import = %+v, %v", res, err)
	}
}

// An import keeps the counts of a never_auto class, and the class stays
// held: the row says why.
func TestImportTrustKeepsNeverAutoClassesHeld(t *testing.T) {
	w := newWorld(t)
	cfg := trustcfg.Defaults()
	cfg.Unattended = false
	cfg.NeverAuto = []string{"trash"}
	f := desk.TrustImportFile{Version: 1, Entries: []desk.TrustImportEntry{
		entry("local", "state", "trash", 99), entry("local", "state", "archive", 99),
	}}
	res, err := w.as(operator).ImportTrust(t.Context(), f, importOpts(w, cfg, false))
	if err != nil || !res.OK() || res.Imported != 2 {
		t.Fatalf("import = %+v, %v", res, err)
	}
	trash, archive := res.Entries[0], res.Entries[1]
	if trash.Clean != 99 || trash.Automatic || trash.BecomesAutomatic || !strings.HasPrefix(trash.HoldReason, trustcfg.NeverAutoReasonPrefix) {
		t.Fatalf("trash row = %+v", trash)
	}
	if !archive.Automatic || res.Automatic != 1 {
		t.Fatalf("archive row = %+v, becoming automatic %d", archive, res.Automatic)
	}
	if e, ok := storedTrust(t, w, trash.Key); !ok || e.Clean != 99 {
		t.Fatalf("stored trash trust = %+v, %v", e, ok)
	}
}
