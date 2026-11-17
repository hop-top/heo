package e2e

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/model"
)

type adapterVerbsJSON struct {
	Type  string
	Verbs []string
}

// TestAdapterVerbsOffline: the adapters the release binary carries say
// which heo verbs their declared capabilities enable, from the binary
// alone with no daemon: the maintained adapters that declare edit,
// delete and react list the message and reaction verbs, whatsapp only
// the reactions, zendesk the field update, the mail adapters of Gmail and Outlook the tag change, github the status change and the field update, slack the message and
// reaction verbs, history, interaction responses and the three modal verbs, telegram the same
// without history, teams only the message and reaction verbs, and the
// ones that declare none an empty list.
func TestAdapterVerbsOffline(t *testing.T) {
	s := newSandbox(t)
	r := s.run(nil, "adapter", "list", "--format", "json")
	s.want(r, 0, "adapter", "list")
	var list []adapterVerbsJSON
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	got := map[string][]string{}
	for _, a := range list {
		if a.Verbs == nil {
			t.Errorf("%s: no verbs list", a.Type)
		}
		got[a.Type] = a.Verbs
	}
	all := []string{"message edit", "message delete", "reaction add", "reaction remove"}
	// state is the nine conversation state verbs the mail adapters' five
	// state capabilities enable; slack.chat declares archive only.
	state := []string{"conversation archive", "conversation unarchive", "conversation move", "conversation read", "conversation unread",
		"conversation flag", "conversation unflag", "conversation trash", "conversation untrash"}
	for typ, want := range map[string][]string{
		"github.issues":     append(slices.Clone(all), "history fetch", "conversation status", "conversation update"),
		"google.mail":       slices.Concat([]string{"message raw", "history fetch", "conversation tag", "message reply-draft", "conversation unsubscribe"}, state),
		"microsoft.mail":    slices.Concat([]string{"message raw", "history fetch", "conversation tag", "message reply-draft", "conversation unsubscribe"}, state),
		"native.webwidget":  append(slices.Clone(all), "receipt send", "interaction respond"),
		"meta.whatsapp":     {"reaction add", "reaction remove"},
		"zendesk.tickets":   {"conversation update"},
		"smtp.mail":         {},
		"twilio.sms":        {"history fetch"},
		"slack.chat":        append(slices.Clone(all), "history fetch", "interaction respond", "interaction open-modal", "interaction push-modal", "interaction update-modal", "conversation archive", "conversation unarchive"),
		"telegram.messages": append(slices.Clone(all), "interaction respond"),
		"relay.messages":    {},
		"microsoft.teams":   all,
		"gitlab.issues":     {"message edit", "message delete", "history fetch", "conversation status"},
		"jira.issues":       {"message edit", "message delete"},
		"linear.issues":     {"message edit", "message delete"},
	} {
		if g, ok := got[typ]; !ok || !slices.Equal(g, want) {
			t.Errorf("adapter list: %s verbs = %v (listed %v), want %v", typ, g, ok, want)
		}
	}
	var one adapterVerbsJSON
	r = s.run(nil, "adapter", "get", "meta.whatsapp", "--format", "json")
	s.want(r, 0, "adapter", "get")
	if err := json.Unmarshal([]byte(r.stdout), &one); err != nil || !slices.Equal(one.Verbs, []string{"reaction add", "reaction remove"}) {
		t.Errorf("adapter get meta.whatsapp = %+v, %v", one, err)
	}
	r = s.run(nil, "adapter", "get", "github.issues")
	s.want(r, 0, "adapter", "get")
	if !strings.Contains(r.stdout, "verbs") || !strings.Contains(r.stdout, "message edit, message delete, reaction add, reaction remove, history fetch, conversation status, conversation update") {
		t.Errorf("text adapter get lacks the verbs:\n%s", r.stdout)
	}
	r = s.run(nil, "adapter", "list")
	s.want(r, 0, "adapter", "list")
	if !strings.Contains(r.stdout, "VERBS") {
		t.Errorf("text adapter list has no VERBS column:\n%s", r.stdout)
	}
}

type verbStateJSON struct {
	Verb, Capability, State, Reason, Detail string
}

type channelViewJSON struct {
	Key   string
	Type  string
	Verbs []verbStateJSON
	// RequiredPermissions is nil when the key is absent.
	RequiredPermissions []string `json:"required_permissions"`
}

type verbLiveJSON struct {
	Verb, Capability, State, Code string
	Exit                          int
}

func verbOf[T interface{ name() string }](list []T, verb string) (T, bool) {
	for _, v := range list {
		if v.name() == verb {
			return v, true
		}
	}
	var zero T
	return zero, false
}

func (v verbStateJSON) name() string { return v.Verb }
func (v verbLiveJSON) name() string  { return v.Verb }

// TestChannelVerbsOfflineAndLive: `channel get` says, per verb, whether
// the channel's adapter declares what it needs, and why not, without
// opening the adapter: a channel whose adapter fails to open still
// answers. `channel check --remote` says what the provider allows now,
// verb by verb: available, unauthorized (the account lacks the
// permission), disabled (the adapter's configuration switched it off,
// DISABLED, exit 4), unsupported (UNSUPPORTED, exit 2).
func TestChannelVerbsOfflineAndLive(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		fx := c.fakeOf(adapterstest.EffectsType, "fx", adapterstest.Config{Account: "acct-1", Disabled: []model.Capability{model.CapabilityEdit},
			Denied: []model.Capability{model.CapabilityDelete}})
		c.fakeOf(adapterstest.Type, "plain", adapterstest.Config{Account: "acct-2"})
		c.fakeOf(adapterstest.VerbsType, "vb", adapterstest.Config{Account: "acct-5"})
		c.fakeOf(adapterstest.ActsType, "ac", adapterstest.Config{Account: "acct-4"})
		c.fakeOf(adapterstest.UpdatesType, "up", adapterstest.Config{Account: "acct-6"})
		c.fakeOf(adapterstest.LabelsType, "lb", adapterstest.Config{Account: "acct-7"})
		c.fakeOf(adapterstest.DraftsType, "dr", adapterstest.Config{Account: "acct-9"})
		c.fakeOf(adapterstest.UnsubscribeType, "un", adapterstest.Config{Account: "acct-10"})
		c.fakeOf(adapterstest.ViewsType, "vw", adapterstest.Config{Account: "acct-8"})
		c.fakeOf(adapterstest.StateType, "stt", adapterstest.Config{Account: "acct-11"})
		c.fakeOf(adapterstest.StateFolderType, "stf", adapterstest.Config{Account: "acct-12"})
		c.fakeOf(adapterstest.EffectsType, "broken", adapterstest.Config{Account: "acct-3", Broken: true})
		var gh channelOutJSON
		c.json(&gh, "channel", "create", "--type", "github.issues", "--key", "gh", "--configuration", `{}`, "--credential-ref", "gh-token")

		view := func(key string) channelViewJSON {
			t.Helper()
			var v channelViewJSON
			c.json(&v, "channel", "get", key)
			if len(v.Verbs) == 0 || v.Key != key {
				t.Fatalf("channel get %s = %+v", key, v)
			}
			return v
		}
		want := func(v channelViewJSON, verb, capability, state, reason string) {
			t.Helper()
			got, ok := verbOf(v.Verbs, verb)
			if !ok || got.Capability != capability || got.State != state || got.Reason != reason {
				t.Errorf("channel %s verb %s = %+v (listed %v), want %s %s %s", v.Key, verb, got, ok, capability, state, reason)
			}
		}
		// required_permissions is always present: the adapter's list for
		// github.issues, [] for an adapter that lists none.
		var raw map[string]json.RawMessage
		c.json(&raw, "channel", "get", "plain")
		if got := string(raw["required_permissions"]); got != "[]" {
			t.Errorf("plain required_permissions = %q, want []", got)
		}
		if got, want := view("gh").RequiredPermissions, []string{"issues:read", "issues:write", "metadata:read"}; !slices.Equal(got, want) {
			t.Errorf("gh required_permissions = %v, want %v", got, want)
		}
		fxv := view("fx")
		for _, verb := range []string{"message edit", "message delete"} {
			want(fxv, verb, map[string]string{"message edit": "edit", "message delete": "delete"}[verb], "declared", "")
		}
		want(fxv, "reaction add", "react", "declared", "")
		want(fxv, "reaction remove", "react", "declared", "")
		want(fxv, "conversation status", "work_item_act", "unsupported", "not_declared")
		want(fxv, "conversation update", "extension_execute", "unsupported", "not_declared")
		want(fxv, "conversation tag", "extension_execute", "unsupported", "not_declared")
		want(fxv, "message reply-draft", "extension_execute", "unsupported", "not_declared")
		want(fxv, "conversation unsubscribe", "extension_execute", "unsupported", "not_declared")
		for _, verb := range []string{"interaction open-modal", "interaction push-modal", "interaction update-modal"} {
			want(fxv, verb, "extension_execute", "unsupported", "not_declared")
		}
		want(fxv, "participant add", "membership", "unsupported", "not_declared")
		want(fxv, "receipt send", "receipt", "unsupported", "not_declared")
		want(fxv, "interaction respond", "respond", "unsupported", "not_declared")
		want(fxv, "history fetch", "history", "unsupported", "not_declared")
		vbv := view("vb")
		for verb, capability := range map[string]string{"participant add": "membership", "participant remove": "membership", "receipt send": "receipt", "history fetch": "history"} {
			want(vbv, verb, capability, "declared", "")
		}
		want(vbv, "message edit", "edit", "unsupported", "not_declared")
		pv := view("plain")
		for _, verb := range []string{"message edit", "message delete", "reaction add", "reaction remove", "participant add", "participant remove", "receipt send", "history fetch", "interaction respond"} {
			if got, _ := verbOf(pv.Verbs, verb); got.State != "unsupported" || got.Reason != "not_declared" {
				t.Errorf("plain %s = %+v", verb, got)
			}
		}
		// No adapter is opened to say so: a channel that cannot open
		// answers the same.
		want(view("broken"), "message edit", "edit", "declared", "")
		// github.issues without replies: declared, switched off by config.
		ghv := view("gh")
		want(ghv, "message edit", "edit", "unsupported", "config")
		want(ghv, "reaction add", "react", "unsupported", "config")
		if got, _ := verbOf(ghv.Verbs, "message edit"); !strings.Contains(got.Detail, "replies") {
			t.Errorf("github edit detail = %q, want it to name replies", got.Detail)
		}
		// Acts need no replies: the status change is declared with the
		// channel's configuration as it is, and the verbs that need
		// replies are switched off beside it.
		want(ghv, "conversation status", "work_item_act", "declared", "")
		// extension_execute is declared for GitHub's own update, which its
		// adapter authorizes as a work item act: it enables the field update
		// and neither of the other effect verbs.
		want(ghv, "conversation update", "extension_execute", "declared", "")
		want(ghv, "conversation tag", "extension_execute", "unsupported", "not_declared")
		if got, _ := verbOf(ghv.Verbs, "conversation tag"); !strings.Contains(got.Detail, "google.mail.labels") || !strings.Contains(got.Detail, "microsoft.mail.categories") {
			t.Errorf("github conversation tag detail = %q, want it to name both mail effects", got.Detail)
		}
		upv, lbv := view("up"), view("lb")
		want(upv, "conversation update", "extension_execute", "declared", "")
		want(upv, "conversation tag", "extension_execute", "unsupported", "not_declared")
		want(lbv, "conversation tag", "extension_execute", "declared", "")
		want(lbv, "conversation update", "extension_execute", "unsupported", "not_declared")
		if got, _ := verbOf(lbv.Verbs, "conversation update"); !strings.Contains(got.Detail, "work item act") {
			t.Errorf("lb conversation update detail = %q, want it to name the effect it lacks", got.Detail)
		}
		// The reply draft and the unsubscribe each follow their own effect:
		// the label effect enables neither, and each enables only its verb.
		drv, unv := view("dr"), view("un")
		want(drv, "message reply-draft", "extension_execute", "declared", "")
		want(drv, "conversation unsubscribe", "extension_execute", "unsupported", "not_declared")
		want(drv, "conversation tag", "extension_execute", "unsupported", "not_declared")
		want(unv, "conversation unsubscribe", "extension_execute", "declared", "")
		want(unv, "message reply-draft", "extension_execute", "unsupported", "not_declared")
		want(lbv, "message reply-draft", "extension_execute", "unsupported", "not_declared")
		want(lbv, "conversation unsubscribe", "extension_execute", "unsupported", "not_declared")
		if got, _ := verbOf(lbv.Verbs, "conversation unsubscribe"); !strings.Contains(got.Detail, "google.mail.unsubscribe") || !strings.Contains(got.Detail, "microsoft.mail.unsubscribe") {
			t.Errorf("lb conversation unsubscribe detail = %q, want it to name both mail effects", got.Detail)
		}
		// The state verbs follow the capability each pair needs: a mailbox
		// declares all five, a folder-shaped shared provider archive, move and
		// flag only, and no state capability enables an extension verb.
		stv, sfv := view("stt"), view("stf")
		for verb, capab := range map[string]string{
			"conversation archive": "archive", "conversation unarchive": "archive", "conversation move": "move",
			"conversation read": "read_state", "conversation unread": "read_state", "conversation flag": "flag",
			"conversation unflag": "flag", "conversation trash": "trash", "conversation untrash": "trash",
		} {
			want(stv, verb, capab, "declared", "")
			if capab == "read_state" || capab == "trash" {
				want(sfv, verb, capab, "unsupported", "not_declared")
			} else {
				want(sfv, verb, capab, "declared", "")
			}
			want(drv, verb, capab, "unsupported", "not_declared")
		}
		want(stv, "conversation tag", "extension_execute", "unsupported", "not_declared")
		// Slack's modal effects enable the three modal verbs and no other
		// extension verb; the field update's adapter enables none of them.
		vwv := view("vw")
		for _, verb := range []string{"interaction open-modal", "interaction push-modal", "interaction update-modal"} {
			want(vwv, verb, "extension_execute", "declared", "")
			want(upv, verb, "extension_execute", "unsupported", "not_declared")
		}
		want(vwv, "conversation update", "extension_execute", "unsupported", "not_declared")
		if got, _ := verbOf(upv.Verbs, "interaction open-modal"); !strings.Contains(got.Detail, "slack.chat.views.open") {
			t.Errorf("up interaction open-modal detail = %q, want it to name the effect it lacks", got.Detail)
		}
		ac := view("ac")
		want(ac, "conversation status", "work_item_act", "declared", "")
		want(ac, "message edit", "edit", "unsupported", "not_declared")
		c.updateChannelRaw(gh.Channel, `{"replies":true,"account_id":42}`)
		want(view("gh"), "message edit", "edit", "declared", "")

		// The text form.
		r := c.heo(0, "channel", "get", "plain")
		for _, want := range []string{"verb message edit", "unsupported: not_declared (edit)", "verb conversation update"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("channel get text lacks %q:\n%s", want, r.stdout)
			}
		}

		// The live view: what the provider allows now.
		var chk struct {
			Checks []struct {
				Remote struct{ Verbs []verbLiveJSON }
			}
		}
		c.json(&chk, "channel", "check", "fx", "--remote")
		if len(chk.Checks) != 1 {
			t.Fatalf("check = %+v", chk)
		}
		live := chk.Checks[0].Remote.Verbs
		for verb, w := range map[string]verbLiveJSON{
			"message edit":             {State: "disabled", Code: "DISABLED", Exit: 4},
			"message delete":           {State: "unauthorized", Code: "PROVIDER_PERMISSION", Exit: 5},
			"reaction add":             {State: "available"},
			"reaction remove":          {State: "available"},
			"conversation status":      {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"conversation update":      {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"conversation tag":         {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"message reply-draft":      {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"conversation unsubscribe": {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"interaction respond":      {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"interaction open-modal":   {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"interaction update-modal": {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"conversation archive":     {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"conversation move":        {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"conversation read":        {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"conversation flag":        {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"conversation trash":       {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
		} {
			got, ok := verbOf(live, verb)
			if !ok || got.State != w.State || got.Code != w.Code || got.Exit != w.Exit {
				t.Errorf("live %s = %+v (listed %v), want %+v", verb, got, ok, w)
			}
		}
		txt := c.heo(0, "channel", "check", "fx", "--remote")
		for _, want := range []string{"verb", "message edit", "disabled (DISABLED, exit 4)", "unauthorized (PROVIDER_PERMISSION, exit 5)"} {
			if !strings.Contains(txt.stdout, want) {
				t.Errorf("check text lacks %q:\n%s", want, txt.stdout)
			}
		}
		_ = fx
		c.stop(d)
	})
}

// updateChannelRaw replaces ch's configuration through heo.
func (c *cliSite) updateChannelRaw(ch model.Channel, configuration string) {
	c.t.Helper()
	c.updateChannel(ch, configuration)
}
