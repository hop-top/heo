package trustcfg_test

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/trustcfg"
)

var now = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func counts(clean int64) trustcfg.Counts { return trustcfg.Counts{Found: true, Clean: clean} }

func TestDecide(t *testing.T) {
	d := trustcfg.Defaults()
	cases := []struct {
		name     string
		cfg      func(*trustcfg.Config)
		in       trustcfg.Input
		wantHold bool
		reason   string
	}{
		{"fresh type is held (training)", nil, trustcfg.Input{Verb: "state", Class: "read"}, true, "training"},
		{"graduated type runs auto after threshold", nil, trustcfg.Input{Verb: "state", Class: "read", Counts: counts(3)}, false, ""},
		{"one short of threshold still held", nil, trustcfg.Input{Verb: "state", Class: "archive", Counts: counts(2)}, true, "training"},
		{"approval-always mailbox held at any trust",
			func(c *trustcfg.Config) { c.ApprovalAlways = []string{"inb-client"} },
			trustcfg.Input{Channel: "inb-client", Verb: "state", Class: "read", Counts: counts(99)}, true, "approval-always"},
		{"other mailbox unaffected by approval-always",
			func(c *trustcfg.Config) { c.ApprovalAlways = []string{"inb-client"} },
			trustcfg.Input{Channel: "inb-other", Verb: "state", Class: "read", Counts: counts(99)}, false, ""},
		{"correction resets the corrected type only (count zero)", nil, trustcfg.Input{Verb: "state", Class: "archive", Counts: counts(0)}, true, "training"},
		{"correction leaves the other type automatic", nil, trustcfg.Input{Verb: "state", Class: "read", Counts: counts(3)}, false, ""},
		{"unsubscribe is held with no history", nil, trustcfg.Input{Verb: "unsubscribe", Counts: counts(0)}, true, "always held"},
		{"trusted reply without cc runs auto", func(c *trustcfg.Config) { c.Unattended = false },
			trustcfg.Input{Verb: "reply", Class: "draft", Counts: counts(99)}, false, ""},
		{"same reply with cc forced held",
			func(c *trustcfg.Config) { c.Unattended = false },
			trustcfg.Input{Verb: "reply", Class: "draft", Counts: counts(99), Facts: trustcfg.Facts{AddedCC: true}}, true, "cc"},
		{"trusted interactive send runs auto", func(c *trustcfg.Config) { c.Unattended = false },
			trustcfg.Input{Verb: "reply", Class: "send", Counts: counts(99)}, false, ""},
		{"unattended send forced held", nil,
			trustcfg.Input{Verb: "reply", Class: "send", Counts: counts(99)}, true, "unattended"},
		{"unattended does not hold a draft", nil,
			trustcfg.Input{Verb: "reply", Class: "draft", Counts: counts(99)}, false, ""},
		{"reply-to redirect forced held", func(c *trustcfg.Config) { c.Unattended = false },
			trustcfg.Input{Verb: "reply", Class: "send", Counts: counts(99), Facts: trustcfg.Facts{ReplyToDiffers: true}}, true, "Reply-To"},
		{"carried-over operation stays held", nil,
			trustcfg.Input{Verb: "state", Class: "read", Counts: counts(99), Facts: trustcfg.Facts{CarriedOver: true}}, true, "carried"},
		{"label threshold applies per label", nil, trustcfg.Input{Verb: "tag", Class: "Finance", Counts: counts(3)}, false, ""},
		{"label below threshold held", nil, trustcfg.Input{Verb: "tag", Class: "Sales", Counts: counts(2)}, true, "training"},
		{"move target uses move_to threshold", nil, trustcfg.Input{Verb: "move", Class: "Later", Counts: counts(3)}, false, ""},
		{"lane newsletter graduates at 3", nil, trustcfg.Input{Verb: "lane", Class: "newsletter", Counts: counts(3)}, false, ""},
		{"lane unsolicited needs 5", nil, trustcfg.Input{Verb: "lane", Class: "unsolicited", Counts: counts(4)}, true, "training"},
		{"attention threshold zero is always automatic", nil, trustcfg.Input{Verb: "attention", Counts: counts(0)}, false, ""},
		{"attention still held when mailbox approval-always",
			func(c *trustcfg.Config) { c.ApprovalAlways = []string{"x"} },
			trustcfg.Input{Channel: "x", Verb: "attention"}, true, "approval-always"},
		{"unknown class defaults to 15", nil, trustcfg.Input{Verb: "edit", Class: "message", Counts: counts(14)}, true, "training"},
		{"unknown class graduates at 15", nil, trustcfg.Input{Verb: "edit", Class: "message", Counts: counts(15)}, false, ""},
		{"reply send needs 8", func(c *trustcfg.Config) { c.Unattended = false },
			trustcfg.Input{Verb: "reply", Class: "send", Counts: counts(7)}, true, "training"},
		{"reply draft needs 5", nil, trustcfg.Input{Verb: "reply", Class: "draft", Counts: counts(5)}, false, ""},
		{"trash needs 5", nil, trustcfg.Input{Verb: "state", Class: "trash", Counts: counts(4)}, true, "training"},
		{"spam needs 5", nil, trustcfg.Input{Verb: "state", Class: "spam", Counts: counts(5)}, false, ""},
		{"star needs 3", nil, trustcfg.Input{Verb: "state", Class: "star", Counts: counts(3)}, false, ""},
		{"unstar needs 3", nil, trustcfg.Input{Verb: "state", Class: "unstar", Counts: counts(2)}, true, "training"},
		{"unread needs 3", nil, trustcfg.Input{Verb: "state", Class: "unread", Counts: counts(3)}, false, ""},
		{"unsubscribe is held after 5 clean approvals", nil, trustcfg.Input{Verb: "unsubscribe", Counts: counts(5)}, true, "always held"},
		{"unsubscribe is held after 1000 clean approvals", nil, trustcfg.Input{Verb: "unsubscribe", Counts: counts(1000)}, true, "always held"},
		{"unsubscribe threshold override cannot make it automatic",
			func(c *trustcfg.Config) { c.Thresholds["unsubscribe"] = 0 },
			trustcfg.Input{Verb: "unsubscribe", Counts: counts(99)}, true, "always held"},
		{"unsubscribe threshold 1 cannot make it automatic",
			func(c *trustcfg.Config) { c.Thresholds["unsubscribe"] = 1 },
			trustcfg.Input{Verb: "unsubscribe", Counts: counts(99)}, true, "always held"},
		{"unsubscribe seed cannot make it automatic",
			func(c *trustcfg.Config) { c.Seed = []string{"unsubscribe"} },
			trustcfg.Input{Verb: "unsubscribe"}, true, "always held"},
		{"unsubscribe held while unattended is off", func(c *trustcfg.Config) { c.Unattended = false },
			trustcfg.Input{Verb: "unsubscribe", Counts: counts(99)}, true, "always held"},
		{"configured threshold overrides the default",
			func(c *trustcfg.Config) { c.Thresholds["archive"] = 1 },
			trustcfg.Input{Verb: "state", Class: "archive", Counts: counts(1)}, false, ""},
		{"exact class beats its base",
			func(c *trustcfg.Config) { c.Thresholds["label:finance"] = 9 },
			trustcfg.Input{Verb: "label", Class: "Finance", Counts: counts(3)}, true, "training"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := trustcfg.Config{Thresholds: trustcfg.DefaultThresholds(), Unattended: d.Unattended, Supervised: d.Supervised}
			if c.cfg != nil {
				c.cfg(&cfg)
			}
			c.in.Now = now
			got := cfg.Decide(c.in)
			if got.Hold != c.wantHold || !strings.Contains(got.Reason, c.reason) {
				t.Fatalf("Decide = %+v, want hold=%v reason~%q", got, c.wantHold, c.reason)
			}
		})
	}
}

func TestDefaultThresholdTable(t *testing.T) {
	want := map[string]int{
		"read": 3, "unread": 3, "star": 3, "unstar": 3, "label": 3, "archive": 3, "move_to": 3,
		"reply:draft": 5, "trash": 5, "spam": 5, "reply:send": 8, "attention": 0,
		"lane:unsolicited": 5, "lane:newsletter": 3,
	}
	got := trustcfg.DefaultThresholds()
	if len(got) != len(want) {
		t.Fatalf("table = %v", got)
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("%s = %d, want %d", k, got[k], n)
		}
	}
	if _, ok := got["unsubscribe"]; ok {
		t.Fatal("unsubscribe has a threshold: it is always held")
	}
	if trustcfg.DefaultThreshold != 15 {
		t.Fatalf("default = %d", trustcfg.DefaultThreshold)
	}
}

func seeded() trustcfg.Config {
	c := trustcfg.Defaults()
	c.Seed = []string{"read", "unread", "archive"}
	c.Unattended = false
	return c
}

func TestSeed(t *testing.T) {
	cases := []struct {
		name     string
		verb, cl string
		counts   trustcfg.Counts
		wantHold bool
	}{
		{"read automatic for a new person", "state", "read", trustcfg.Counts{}, false},
		{"unread automatic for a new person", "state", "unread", trustcfg.Counts{}, false},
		{"archive automatic for a new person", "state", "archive", trustcfg.Counts{}, false},
		{"label held for a new person", "tag", "Finance", trustcfg.Counts{}, true},
		{"trash held for a new person", "state", "trash", trustcfg.Counts{}, true},
		{"unsubscribe held for a new person", "unsubscribe", "", trustcfg.Counts{}, true},
		{"move held for a new person", "move", "Later", trustcfg.Counts{}, true},
		{"draft held for a new person", "reply", "draft", trustcfg.Counts{}, true},
		{"send held for a new person", "reply", "send", trustcfg.Counts{}, true},
		{"seeded class held again after a correction", "state", "archive", trustcfg.Counts{Found: true, Corrections: 1}, true},
		{"seeded class automatic with clean approvals on record", "state", "archive", trustcfg.Counts{Found: true, Clean: 1}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := seeded().Decide(trustcfg.Input{Verb: c.verb, Class: c.cl, Counts: c.counts, Now: now})
			if got.Hold != c.wantHold {
				t.Fatalf("Decide = %+v", got)
			}
		})
	}
}

func TestSeedValidation(t *testing.T) {
	for _, bad := range []string{"label", "trash", "unsubscribe", "move_to", "reply:draft", "reply:send", "star"} {
		c := trustcfg.Defaults()
		c.Seed = []string{bad}
		if err := c.Validate(); err == nil {
			t.Fatalf("seed %q accepted", bad)
		}
	}
	c := trustcfg.Defaults()
	c.Seed = []string{"read", "unread", "archive"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestUnsubscribeThresholdIsRefused(t *testing.T) {
	c := trustcfg.Defaults()
	c.Thresholds["unsubscribe"] = 1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "unsubscribe") {
		t.Fatalf("Validate = %v", err)
	}
	v := viper.New()
	v.Set("held_operations.trust", map[string]any{"thresholds": map[string]any{"Unsubscribe": 0}})
	if _, err := trustcfg.Load(v); err == nil || !strings.Contains(err.Error(), "always held") {
		t.Fatalf("Load = %v", err)
	}
	if _, err := trustcfg.Load(viper.New()); err != nil {
		t.Fatalf("defaults: %v", err)
	}
}

func TestSupervisedFirstWeek(t *testing.T) {
	start := now.Add(-3 * 24 * time.Hour)
	mk := func(enabled bool, started map[string]string) trustcfg.Config {
		c := seeded()
		c.Supervised.Enabled, c.Supervised.Started = enabled, started
		return c
	}
	trusted := func(cfg trustcfg.Config, verb, class string, at time.Time) trustcfg.Decision {
		return cfg.Decide(trustcfg.Input{OnBehalfOf: "uid:1001", Channel: "inb", Verb: verb, Class: class, Counts: counts(99), Now: at})
	}
	on := mk(true, map[string]string{"uid:1001/inb": start.Format(time.RFC3339)})
	for _, k := range [][2]string{{"state", "read"}, {"state", "archive"}, {"tag", "Finance"}, {"reply", "draft"}, {"state", "trash"}} {
		if d := trusted(on, k[0], k[1], now); !d.Hold || !strings.Contains(d.Reason, "supervised") {
			t.Fatalf("%v inside the week: %+v", k, d)
		}
	}
	if d := trusted(on, "state", "read", start.AddDate(0, 0, 7).Add(-time.Second)); !d.Hold {
		t.Fatalf("last second of the week: %+v", d)
	}
	if d := trusted(on, "state", "read", start.AddDate(0, 0, 7)); d.Hold {
		t.Fatalf("after the week: %+v", d)
	}
	if d := trusted(mk(true, nil), "state", "read", now); !d.Hold {
		t.Fatalf("no start recorded: %+v", d)
	}
	other := mk(true, map[string]string{"uid:2002/inb": start.Format(time.RFC3339)})
	if d := trusted(other, "state", "read", now); !d.Hold {
		t.Fatalf("another person's start: %+v", d)
	}
	if d := trusted(mk(false, map[string]string{"uid:1001/inb": start.Format(time.RFC3339)}), "state", "read", now); d.Hold {
		t.Fatalf("opt-in off: %+v", d)
	}
	if d := mk(true, nil).Decide(trustcfg.Input{OnBehalfOf: "uid:1", Channel: "i", Verb: "state", Class: "read", Now: now}); !d.Hold {
		t.Fatalf("seeded class inside supervised week: %+v", d)
	}
}

func TestLoad(t *testing.T) {
	v := viper.New()
	c, err := trustcfg.Load(v)
	if err != nil || c.Enabled || !c.Unattended || c.Supervised.Enabled || c.Supervised.Days != 7 || c.Thresholds["reply:send"] != 8 {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	v.Set("held_operations.trust", map[string]any{
		"enabled": true, "unattended": false, "seed": []string{"read"},
		"approval_always": []string{"inb-1"},
		"thresholds":      map[string]any{"archive": 1, "Reply:Send": 20},
		"supervised":      map[string]any{"enabled": true, "days": 14, "started": map[string]any{"uid:1/inb": "2026-11-01T00:00:00Z"}},
	})
	c, err = trustcfg.Load(v)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Enabled || c.Unattended || c.Seed[0] != "read" || c.ApprovalAlways[0] != "inb-1" || c.Thresholds["archive"] != 1 ||
		c.Thresholds["reply:send"] != 20 || c.Thresholds["trash"] != 5 || !c.Supervised.Enabled || c.Supervised.Days != 14 {
		t.Fatalf("loaded = %+v", c)
	}
	v.Set("held_operations.trust.seed", []string{"label"})
	if _, err := trustcfg.Load(v); err == nil {
		t.Fatal("seeding label accepted")
	}
}

func TestNeverAuto(t *testing.T) {
	cfg := func(c *trustcfg.Config) { c.Unattended = false; c.NeverAuto = []string{"trash", "spam"} }
	cases := []struct {
		name     string
		cfg      func(*trustcfg.Config)
		in       trustcfg.Input
		wantHold bool
		reason   string
	}{
		{"trash at 99 stays held", cfg, trustcfg.Input{Verb: "state", Class: "trash", Counts: counts(99)}, true, trustcfg.NeverAutoReasonPrefix},
		{"spam at 99 stays held", cfg, trustcfg.Input{Verb: "state", Class: "spam", Counts: counts(99)}, true, trustcfg.NeverAutoReasonPrefix},
		{"a move into trash is trash", cfg, trustcfg.Input{Verb: "state", Class: "trash", Counts: counts(5)}, true, "never automatic"},
		{"archive unaffected", cfg, trustcfg.Input{Verb: "state", Class: "archive", Counts: counts(3)}, false, ""},
		{"unlisted trash earns trust", func(c *trustcfg.Config) { c.Unattended = false }, trustcfg.Input{Verb: "state", Class: "trash", Counts: counts(5)}, false, ""},
		{"base class covers its subclasses",
			func(c *trustcfg.Config) { c.NeverAuto = []string{"label"} },
			trustcfg.Input{Verb: "tag", Class: "Finance", Counts: counts(99)}, true, "never automatic"},
		{"unsubscribe keeps its own reason", cfg, trustcfg.Input{Verb: "unsubscribe", Counts: counts(99)}, true, trustcfg.UnsubscribeReason},
		{"seed cannot lift the ceiling",
			func(c *trustcfg.Config) { c.NeverAuto = []string{"archive"}; c.Seed = []string{"archive"} },
			trustcfg.Input{Verb: "state", Class: "archive"}, true, "never automatic"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conf := trustcfg.Defaults()
			c.cfg(&conf)
			c.in.Now = now
			got := conf.Decide(c.in)
			if got.Hold != c.wantHold || !strings.Contains(got.Reason, c.reason) {
				t.Fatalf("Decide = %+v, want hold=%v reason~%q", got, c.wantHold, c.reason)
			}
		})
	}
}

func TestNeverAutoValidate(t *testing.T) {
	for name, mut := range map[string]func(*trustcfg.Config){
		"empty entry":            func(c *trustcfg.Config) { c.NeverAuto = []string{" "} },
		"unsubscribe is already": func(c *trustcfg.Config) { c.NeverAuto = []string{"unsubscribe"} },
		"seeded class":           func(c *trustcfg.Config) { c.NeverAuto = []string{"archive"}; c.Seed = []string{"archive"} },
		"always-automatic class": func(c *trustcfg.Config) { c.NeverAuto = []string{"attention"} },
		"zero threshold":         func(c *trustcfg.Config) { c.NeverAuto = []string{"trash"}; c.Thresholds["trash"] = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			c := trustcfg.Defaults()
			mut(&c)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "never_auto") {
				t.Fatalf("Validate = %v, want a never_auto error", err)
			}
		})
	}
	c := trustcfg.Defaults()
	c.NeverAuto = []string{"trash", "SPAM"}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid ceiling refused: %v", err)
	}
}

func TestLoadNeverAuto(t *testing.T) {
	v := viper.New()
	v.Set("held_operations.trust", map[string]any{"enabled": true, "never_auto": []string{"Trash", "spam"}})
	c, err := trustcfg.Load(v)
	if err != nil || len(c.NeverAuto) != 2 || c.NeverAuto[0] != "trash" {
		t.Fatalf("loaded = %+v, %v", c.NeverAuto, err)
	}
	v.Set("held_operations.trust.never_auto", []string{"unsubscribe"})
	if _, err := trustcfg.Load(v); err == nil {
		t.Fatal("never_auto unsubscribe accepted")
	}
}

func TestLevel(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(*trustcfg.Config)
		in   trustcfg.Input
		want trustcfg.Level
	}{
		{"count 0 asks each", nil, trustcfg.Input{Verb: "state", Class: "archive", Counts: counts(0)}, trustcfg.LevelAskEach},
		{"no counter asks each", nil, trustcfg.Input{Verb: "state", Class: "archive"}, trustcfg.LevelAskEach},
		{"count 1 asks in batches", nil, trustcfg.Input{Verb: "state", Class: "archive", Counts: counts(1)}, trustcfg.LevelAskInBatches},
		{"one short of the threshold asks in batches", nil, trustcfg.Input{Verb: "state", Class: "archive", Counts: counts(2)}, trustcfg.LevelAskInBatches},
		{"at the threshold is auto", nil, trustcfg.Input{Verb: "state", Class: "archive", Counts: counts(3)}, trustcfg.LevelAuto},
		{"above the threshold is auto", nil, trustcfg.Input{Verb: "state", Class: "archive", Counts: counts(40)}, trustcfg.LevelAuto},
		{"threshold 0 is auto", nil, trustcfg.Input{Verb: "attention"}, trustcfg.LevelAuto},
		{"seeded with no correction is auto", func(c *trustcfg.Config) { c.Seed = []string{"archive"} },
			trustcfg.Input{Verb: "state", Class: "archive"}, trustcfg.LevelAuto},
		{"seeded but corrected asks again", func(c *trustcfg.Config) { c.Seed = []string{"archive"} },
			trustcfg.Input{Verb: "state", Class: "archive", Counts: trustcfg.Counts{Found: true, Corrections: 1}}, trustcfg.LevelAskEach},
		{"never_auto at 99", func(c *trustcfg.Config) { c.NeverAuto = []string{"trash"} },
			trustcfg.Input{Verb: "state", Class: "trash", Counts: counts(99)}, trustcfg.LevelNeverAuto},
		{"never_auto at 0", func(c *trustcfg.Config) { c.NeverAuto = []string{"trash"} },
			trustcfg.Input{Verb: "state", Class: "trash"}, trustcfg.LevelNeverAuto},
		{"unsubscribe is never auto", nil, trustcfg.Input{Verb: "unsubscribe", Counts: counts(99)}, trustcfg.LevelNeverAuto},
		{"approval-always mailbox is never auto", func(c *trustcfg.Config) { c.ApprovalAlways = []string{"vip"} },
			trustcfg.Input{Channel: "vip", Verb: "state", Class: "archive", Counts: counts(9)}, trustcfg.LevelNeverAuto},
		{"unattended send is never auto", func(c *trustcfg.Config) { c.Unattended = true },
			trustcfg.Input{Verb: "reply", Class: "send", Counts: counts(9)}, trustcfg.LevelNeverAuto},
		{"the supervised week does not move the level", func(c *trustcfg.Config) { c.Supervised.Enabled = true },
			trustcfg.Input{Verb: "state", Class: "archive", Counts: counts(3)}, trustcfg.LevelAuto},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conf := trustcfg.Defaults()
			conf.Unattended = false
			if c.cfg != nil {
				c.cfg(&conf)
			}
			c.in.Now = now
			if got := conf.Level(c.in); got != c.want {
				t.Fatalf("Level = %s, want %s", got, c.want)
			}
		})
	}
	if trustcfg.LevelAskInBatches.Label() != "Ask in batches" || trustcfg.LevelNeverAuto.Label() != "Never auto" ||
		trustcfg.LevelAskEach.Label() != "Ask each" || trustcfg.LevelAuto.Label() != "Auto" {
		t.Fatal("labels")
	}
}

// Level and Decide agree: a key the ladder runs automatically is at level
// auto or held only by the supervised week; one at another level is held.
func TestLevelAgreesWithDecide(t *testing.T) {
	cfg := trustcfg.Defaults()
	cfg.Unattended = false
	cfg.NeverAuto = []string{"trash"}
	cfg.Seed = []string{"read"}
	for _, verb := range [][2]string{{"state", "read"}, {"state", "archive"}, {"state", "trash"}, {"reply", "send"}, {"unsubscribe", ""}, {"edit", "message"}} {
		for n := int64(0); n < 20; n++ {
			in := trustcfg.Input{Verb: verb[0], Class: verb[1], Counts: counts(n), Now: now}
			auto := !cfg.Decide(in).Hold
			if lv := cfg.Level(in); auto != (lv == trustcfg.LevelAuto) {
				t.Fatalf("%v at %d: Decide automatic=%v, Level=%s", verb, n, auto, lv)
			}
		}
	}
}

func TestThresholdOf(t *testing.T) {
	cfg := trustcfg.Defaults()
	if n := cfg.ThresholdOf("", "state", "trash"); n == nil || *n != 5 {
		t.Fatalf("trash = %v", n)
	}
	if n := cfg.ThresholdOf("", "edit", "message"); n == nil || *n != 15 {
		t.Fatalf("edit:message = %v", n)
	}
	if n := cfg.ThresholdOf("", "unsubscribe", ""); n != nil {
		t.Fatalf("unsubscribe = %d, want none", *n)
	}
}

// The classes of chat, messaging and tracker verbs take heo's fallback
// until a tenant sets them: no built-in opinion on reactions, statuses or
// edits.
func TestChatAndTrackerClassesTakeTheFallback(t *testing.T) {
	cfg := trustcfg.Defaults()
	for _, c := range [][2]string{{"react", "message"}, {"edit", "message"}, {"delete", "message"}, {"status", "done"},
		{"fields", "priority"}, {"note", "local"}, {"note", "provider"}, {"start", ""}} {
		if got := cfg.ThresholdFor("any-inbox", trustcfg.Class(c[0], c[1])); got != trustcfg.DefaultThreshold {
			t.Errorf("%s:%s = %d, want the fallback %d", c[0], c[1], got, trustcfg.DefaultThreshold)
		}
	}
	for _, class := range []string{"react:message", "edit:message", "delete:message", "status", "note", "fields"} {
		if _, ok := trustcfg.DefaultThresholds()[class]; ok {
			t.Errorf("%s ships a built-in threshold", class)
		}
	}
}

// The class names of the verbs a tenant may set a threshold for.
func TestClassNamingScheme(t *testing.T) {
	for want, in := range map[string][2]string{
		"react:message": {"react", "message"}, "archive": {"state", "archive"}, "status:done": {"status", "Done"},
		"edit:message": {"edit", "message"}, "delete:message": {"delete", "message"}, "reply:send": {"reply", "send"},
		"note:local": {"note", "local"}, "note:provider": {"note", "provider"}, "fields:a,b": {"fields", "a,b"},
	} {
		if got := trustcfg.Class(in[0], in[1]); got != want {
			t.Errorf("Class(%s, %s) = %s, want %s", in[0], in[1], got, want)
		}
	}
}

// A tenant's policy for a supervised personal inbox, as the documentation
// gives it as an example: each class earns as configured.
func exampleConfig() trustcfg.Config {
	c := trustcfg.Defaults()
	c.Unattended = false
	for k, n := range map[string]int{"react:message": 3, "archive": 3, "status": 5, "edit:message": 15, "delete:message": 15, "reply:send": 8} {
		c.Thresholds[k] = n
	}
	return c
}

func TestExamplePolicyEarnsAsConfigured(t *testing.T) {
	cfg := exampleConfig()
	for _, c := range []struct {
		verb, class string
		need        int64
	}{{"react", "message", 3}, {"state", "archive", 3}, {"status", "done", 5}, {"edit", "message", 15}, {"delete", "message", 15}, {"reply", "send", 8}} {
		t.Run(trustcfg.Class(c.verb, c.class), func(t *testing.T) {
			below := cfg.Decide(trustcfg.Input{Verb: c.verb, Class: c.class, Counts: counts(c.need - 1), Now: now})
			at := cfg.Decide(trustcfg.Input{Verb: c.verb, Class: c.class, Counts: counts(c.need), Now: now})
			if !below.Hold || !strings.Contains(below.Reason, "training") || at.Hold {
				t.Fatalf("below %+v at %+v", below, at)
			}
		})
	}
}

func TestInboxThresholdsOverride(t *testing.T) {
	cfg := exampleConfig()
	cfg.Inboxes = map[string]trustcfg.InboxPolicy{"chat": {Thresholds: map[string]int{"react:message": 1, "status": 2}}}
	for _, c := range []struct {
		inbox, class string
		want         int
	}{
		{"chat", "react:message", 1}, {"chat", "status:done", 2}, {"CHAT", "react:message", 1},
		{"other", "react:message", 3}, {"other", "status:done", 5}, {"", "react:message", 3},
		{"chat", "edit:message", 15}, // not overridden: the ladder's
	} {
		if got := cfg.ThresholdFor(c.inbox, c.class); got != c.want {
			t.Errorf("%s %s = %d, want %d", c.inbox, c.class, got, c.want)
		}
	}
	in := trustcfg.Input{Channel: "chat", Verb: "react", Class: "message", Counts: counts(1), Now: now}
	if cfg.Decide(in).Hold {
		t.Fatal("the inbox's threshold of 1 did not apply")
	}
	in.Channel = "other"
	if !cfg.Decide(in).Hold {
		t.Fatal("another inbox took the override")
	}
}

func TestFirstReplyToUnknownContact(t *testing.T) {
	first := func(channel string, n int64) trustcfg.Input {
		return trustcfg.Input{Channel: channel, Verb: "reply", Class: "send", Counts: counts(n), Now: now,
			Facts: trustcfg.Facts{FirstReplyUnknown: true}}
	}
	cfg := trustcfg.Defaults()
	cfg.Unattended = false
	cfg.Inboxes = map[string]trustcfg.InboxPolicy{"responder": {FirstReply: trustcfg.FirstReplyAllow}, "strict": {FirstReply: trustcfg.FirstReplyHold}}

	// The default holds, whatever the count, with its own reason.
	for _, ch := range []string{"plain", "strict", ""} {
		d := cfg.Decide(first(ch, 99))
		if !d.Hold || d.Reason != trustcfg.FirstReplyReason {
			t.Errorf("channel %q: %+v", ch, d)
		}
		if cfg.Level(first(ch, 99)) != trustcfg.LevelNeverAuto {
			t.Errorf("channel %q level = %s", ch, cfg.Level(first(ch, 99)))
		}
	}
	// A channel that allows it follows the ordinary rules.
	if d := cfg.Decide(first("responder", 7)); !d.Hold || !strings.Contains(d.Reason, "training") {
		t.Errorf("allow, below the threshold: %+v", d)
	}
	if d := cfg.Decide(first("responder", 8)); d.Hold {
		t.Errorf("allow, at the threshold: %+v", d)
	}
	if d := cfg.Decide(first("RESPONDER", 8)); d.Hold {
		t.Errorf("allow, inbox ID case: %+v", d)
	}
	// Allow does not skip the other rules.
	cfg.Unattended = true
	if d := cfg.Decide(first("responder", 99)); !d.Hold || d.Reason != "unattended send" {
		t.Errorf("allow while unattended: %+v", d)
	}
	cfg.Unattended = false
	cfg.ApprovalAlways = []string{"responder"}
	if d := cfg.Decide(first("responder", 99)); !d.Hold || !strings.Contains(d.Reason, "approval-always") {
		t.Errorf("allow on an approval-always inbox: %+v", d)
	}
	// A known contact (no fact) is unaffected either way.
	cfg.ApprovalAlways = nil
	for _, ch := range []string{"plain", "responder"} {
		in := first(ch, 8)
		in.FirstReplyUnknown = false
		if d := cfg.Decide(in); d.Hold {
			t.Errorf("known contact on %q held: %+v", ch, d)
		}
	}
	// The fact holds only a reply:send.
	in := trustcfg.Input{Channel: "plain", Verb: "state", Class: "archive", Counts: counts(3), Now: now, Facts: trustcfg.Facts{FirstReplyUnknown: true}}
	if d := cfg.Decide(in); d.Hold {
		t.Errorf("another class held by the fact: %+v", d)
	}
}

func TestInboxPolicyLoadAndValidate(t *testing.T) {
	v := viper.New()
	v.Set("held_operations.trust", map[string]any{"inboxes": map[string]any{
		"Inb-1": map[string]any{"first_reply": "Allow", "thresholds": map[string]any{"React:Message": 2}},
	}})
	c, err := trustcfg.Load(v)
	if err != nil || c.ThresholdFor("inb-1", "react:message") != 2 || c.Inboxes["inb-1"].FirstReply != trustcfg.FirstReplyAllow {
		t.Fatalf("loaded = %+v, %v", c.Inboxes, err)
	}
	for name, in := range map[string]map[string]any{
		"first_reply value": {"first_reply": "sometimes"},
		"negative":          {"thresholds": map[string]any{"status": -1}},
		"unsubscribe":       {"thresholds": map[string]any{"unsubscribe": 1}},
	} {
		v := viper.New()
		v.Set("held_operations.trust", map[string]any{"inboxes": map[string]any{"i": in}})
		if _, err := trustcfg.Load(v); err == nil || !strings.Contains(err.Error(), "inboxes.i") {
			t.Errorf("%s: %v, want an inboxes.i error", name, err)
		}
	}
}
