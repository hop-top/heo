package adapters_test

import (
	"slices"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
	"hop.top/uma/adapter/ticket/github"
	"hop.top/uma/adapter/ticket/gitlab"
	"hop.top/uma/adapter/ticket/jira"
	"hop.top/uma/adapter/ticket/linear"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
)

// TestVerbsFollowDeclarations: the verbs an adapter enables are exactly
// those whose capability it declares, in documentation order; a capability
// no heo verb uses enables none.
func TestVerbsFollowDeclarations(t *testing.T) {
	plain := adapterstest.New()
	if got := adapters.VerbsOf(plain.Adapter()); len(got) != 0 {
		t.Errorf("plain fake enables %v, want none", got)
	}
	fx := adapterstest.New()
	fx.DeclareEffects()
	want := []string{"message edit", "message delete", "reaction add", "reaction remove"}
	if got := adapters.VerbsOf(fx.Adapter()); !slices.Equal(got, want) {
		t.Errorf("effects fake enables %v, want %v", got, want)
	}
	// membership, receipt and history each enable their own verbs and
	// nothing else.
	for _, tc := range []struct {
		declare func(*adapterstest.Factory)
		want    []string
	}{
		{(*adapterstest.Factory).DeclareMembership, []string{"participant add", "participant remove"}},
		{(*adapterstest.Factory).DeclareReceipts, []string{"receipt send"}},
		{(*adapterstest.Factory).DeclareHistory, []string{"history fetch"}},
	} {
		f := adapterstest.New()
		tc.declare(f)
		if got := adapters.VerbsOf(f.Adapter()); !slices.Equal(got, tc.want) {
			t.Errorf("fake enables %v, want %v", got, tc.want)
		}
	}
	// A fake that reports interactions declares no respond: it enables no
	// verb, and `interaction respond` follows the respond capability alone.
	reports := adapterstest.New()
	reports.DeclareInteractions()
	if got := adapters.VerbsOf(reports.Adapter()); len(got) != 0 {
		t.Errorf("a fake that only reports interactions enables %v, want none", got)
	}
	if got := adapters.VerbsFor(model.CapabilityRespond); !slices.Equal(got, []string{"interaction respond"}) {
		t.Errorf("respond enables %v", got)
	}
	if got := adapters.VerbsFor(model.CapabilityRespondFollowUp); len(got) != 0 {
		t.Errorf("respond_follow_up enables %v: a follow-up is a flag of interaction respond, not a verb", got)
	}
	if got := adapters.VerbsFor(model.CapabilityReact); !slices.Equal(got, []string{"reaction add", "reaction remove"}) {
		t.Errorf("react enables %v", got)
	}
	if got := adapters.VerbsFor(model.CapabilityReceive); got == nil || len(got) != 0 {
		t.Errorf("receive enables %v, want an empty list", got)
	}
}

// TestVerbStatesOffline: a channel's verb table says declared or
// unsupported with a reason, from the registry and the record alone: an
// adapter that is not in the build is adapter_missing, one that lacks
// the capability not_declared, one the channel's own configuration
// switches off config.
func TestVerbStatesOffline(t *testing.T) {
	r := adapters.NewRegistry()
	plain := adapterstest.New()
	if err := r.Register(plain.Adapter()); err != nil {
		t.Fatal(err)
	}
	fx := adapterstest.New()
	fx.As(adapterstest.EffectsType)
	fx.DeclareEffects()
	a := fx.Adapter()
	a.CapabilityOff = func(ch model.Channel) map[model.Capability]string {
		if ch.Key == "off" {
			return map[model.Capability]string{model.CapabilityEdit: "replies is off"}
		}
		return nil
	}
	if err := r.Register(a); err != nil {
		t.Fatal(err)
	}
	state := func(ch model.Channel, verb string) adapters.VerbState {
		t.Helper()
		for _, v := range r.VerbStates(ch) {
			if v.Verb == verb {
				return v
			}
		}
		t.Fatalf("no verb %q in %+v", verb, r.VerbStates(ch))
		return adapters.VerbState{}
	}
	if len(r.VerbStates(model.Channel{Type: "gone"})) != len(adapters.ProviderVerbs()) {
		t.Error("a missing adapter lists a different set of verbs")
	}
	if got := state(model.Channel{Type: "gone"}, "message edit"); got.State != adapters.VerbUnsupported || got.Reason != adapters.ReasonAdapterMissing {
		t.Errorf("missing adapter: %+v", got)
	}
	if got := state(model.Channel{Type: adapterstest.Type}, "message edit"); got.State != adapters.VerbUnsupported || got.Reason != adapters.ReasonNotDeclared ||
		got.Capability != model.CapabilityEdit {
		t.Errorf("plain fake: %+v", got)
	}
	if got := state(model.Channel{Type: adapterstest.EffectsType}, "message edit"); got.State != adapters.VerbDeclared || got.Reason != "" {
		t.Errorf("effects fake: %+v", got)
	}
	if got := state(model.Channel{Type: adapterstest.EffectsType, Key: "off"}, "message edit"); got.State != adapters.VerbUnsupported ||
		got.Reason != adapters.ReasonConfig || got.Detail != "replies is off" {
		t.Errorf("configured off: %+v", got)
	}
	if got := state(model.Channel{Type: adapterstest.EffectsType, Key: "off"}, "message delete"); got.State != adapters.VerbDeclared {
		t.Errorf("a verb the configuration leaves on: %+v", got)
	}
	// Declared by the adapter does not mean this build runs
	// conversation update: the capability is extension_execute, and the
	// adapter must ship an effect authorized as a work item act.
	if got := state(model.Channel{Type: adapterstest.EffectsType}, "conversation update"); got.Reason != adapters.ReasonNotDeclared {
		t.Errorf("conversation update on a fake: %+v", got)
	}
}

// TestStatusNeedsTheActCapability: `conversation status` needs the
// work_item_act capability; which transitions the adapter performs is
// its declaration, shown as the verb's limits and checked when the verb
// runs, not by the verb table. A fake declaring the issue acts, and one
// declaring another transition (a tracker's resolved), both enable it; an
// adapter declaring no act does not.
func TestStatusNeedsTheActCapability(t *testing.T) {
	acts := adapterstest.New()
	acts.DeclareWorkItemActs()
	if got := adapters.VerbsOf(acts.Adapter()); !slices.Equal(got, []string{"conversation status"}) {
		t.Errorf("issue-act fake enables %v, want conversation status", got)
	}
	if got := adapters.VerbsFor(model.CapabilityWorkItemAct); !slices.Equal(got, []string{"conversation status"}) {
		t.Errorf("work_item_act enables %v", got)
	}
	tracker := adapterstest.New()
	tracker.As(adapterstest.EffectsType)
	tracker.DeclareWorkItemActs(adapter.WorkItemTransition{
		To: model.WorkItemResolved, From: []string{model.WorkItemOpen, model.WorkItemPending}, Reasons: []string{"fixed"}})
	if got := adapters.VerbsOf(tracker.Adapter()); !slices.Equal(got, []string{"conversation status"}) {
		t.Errorf("a tracker declaring only resolved enables %v", got)
	}
	plain := adapterstest.New()
	plain.As("test.heo.fake.noact")
	if got := adapters.VerbsOf(plain.Adapter()); slices.Contains(got, "conversation status") {
		t.Errorf("an adapter declaring no act enables %v", got)
	}

	r := adapters.NewRegistry()
	for _, f := range []*adapterstest.Factory{acts, tracker, plain} {
		if err := r.Register(f.Adapter()); err != nil {
			t.Fatal(err)
		}
	}
	state := func(typ model.TypeKey) adapters.VerbState {
		t.Helper()
		for _, v := range r.VerbStates(model.Channel{Type: typ}) {
			if v.Verb == "conversation status" {
				return v
			}
		}
		t.Fatalf("no conversation status verb for %s", typ)
		return adapters.VerbState{}
	}
	got := state(adapterstest.Type)
	if got.State != adapters.VerbDeclared || got.Capability != model.CapabilityWorkItemAct || len(got.Limits.Transitions) != 2 {
		t.Errorf("issue-act fake: %+v", got)
	}
	got = state(adapterstest.EffectsType)
	if got.State != adapters.VerbDeclared || len(got.Limits.Transitions) != 1 || got.Limits.Transitions[0].To != model.WorkItemResolved {
		t.Errorf("tracker fake: %+v, want declared with its one transition as the limit", got)
	}
	if got := state("test.heo.fake.noact"); got.State != adapters.VerbUnsupported || got.Reason != adapters.ReasonNotDeclared {
		t.Errorf("no-act fake: %+v", got)
	}
}

// TestUpdateNeedsAWorkItemUpdateEffect: `conversation update` needs
// extension_execute and an effect the adapter ships authorized as a work
// item act, whatever its name: GitHub's and Zendesk's both qualify, found
// by that declaration alone. An adapter whose effects are authorized
// otherwise (the mail labels) or that ships none enables it not, and
// nothing without the effects registered beside the factory.
func TestUpdateNeedsAWorkItemUpdateEffect(t *testing.T) {
	account := func(model.Channel) (string, error) { return "acct", nil }
	gh := github.NewFactory(github.Options{})
	ghAdapter := adapters.Adapter{Factory: gh, Account: account, Extensions: gh.Extensions()}
	zd := zendesk.NewFactory()
	zdAdapter := adapters.Adapter{Factory: zd, Account: account, Extensions: zd.Extensions()}
	g := google.NewFactory(google.Options{})
	gAdapter := adapters.Adapter{Factory: g, Account: account, Extensions: g.Extensions()}
	for name, a := range map[string]adapters.Adapter{"github": ghAdapter, "zendesk": zdAdapter} {
		if got := adapters.VerbsOf(a); !slices.Contains(got, "conversation update") {
			t.Errorf("%s enables %v, want conversation update", name, got)
		}
		bare := a
		bare.Extensions = nil
		if got := adapters.VerbsOf(bare); slices.Contains(got, "conversation update") {
			t.Errorf("%s without its effects enables %v", name, got)
		}
	}
	if got := adapters.VerbsOf(gAdapter); slices.Contains(got, "conversation update") {
		t.Errorf("google.mail enables %v: its effects are not work item updates", got)
	}
	if got := adapters.UpdateEffects(gh.Extensions()); len(got) != 1 || got[0].Name != github.ExtensionUpdate {
		t.Errorf("github update effects = %+v", got)
	}
	if got := adapters.UpdateEffects(zd.Extensions()); len(got) != 1 || got[0].Name != zendesk.ExtensionUpdate {
		t.Errorf("zendesk update effects (its internal note is authorized as a note) = %+v", got)
	}

	r := adapters.NewRegistry()
	for _, a := range []adapters.Adapter{ghAdapter, zdAdapter, gAdapter} {
		if err := r.Register(a); err != nil {
			t.Fatal(err)
		}
	}
	state := func(typ model.TypeKey) adapters.VerbState {
		t.Helper()
		for _, v := range r.VerbStates(model.Channel{Type: typ}) {
			if v.Verb == "conversation update" {
				return v
			}
		}
		t.Fatalf("no conversation update verb for %s", typ)
		return adapters.VerbState{}
	}
	for typ, effect := range map[model.TypeKey]string{github.Type: github.ExtensionUpdate, zendesk.Type: zendesk.ExtensionUpdate} {
		got := state(typ)
		if got.State != adapters.VerbDeclared || got.Reason != "" || !slices.Equal(got.Limits.Effects, []string{effect}) {
			t.Errorf("%s: %+v, want declared with %s as its effect", typ, got, effect)
		}
	}
	if got := state(google.Type); got.State != adapters.VerbUnsupported || got.Reason != adapters.ReasonNotDeclared ||
		got.Capability != model.CapabilityExtensionExecute || !strings.Contains(got.Detail, "work item act") {
		t.Errorf("google.mail: %+v, want unsupported not_declared naming the missing effect", got)
	}
}

// TestActDeclared: heo reads a descriptor's work item acts as uma does
// at submission, so a preview never disagrees with it: the target, the
// source status and the reason must each be declared, and nothing is
// coerced.
func TestActDeclared(t *testing.T) {
	d := adapterstest.New()
	d.DeclareWorkItemActs()
	desc := d.Describe()
	for _, tc := range []struct {
		from, to, reason string
		want             bool
	}{
		{"open", "closed", "", true},
		{"open", "closed", "completed", true},
		{"open", "closed", "not_planned", true},
		{"closed", "open", "", true},
		{"closed", "open", "reopened", true},
		{"open", "closed", "duplicate", false},
		{"open", "open", "", false},
		{"closed", "closed", "", false},
		{"open", "resolved", "", false},
		{"", "closed", "", false},
		{"open", "closed", "reopened", false},
	} {
		err := adapters.ActDeclared(desc, tc.from, tc.to, tc.reason)
		if (err == nil) != tc.want || (err != nil && !model.IsCode(err, model.Unsupported)) {
			t.Errorf("%s -> %s (%q): %v, want declared=%v and unsupported otherwise", tc.from, tc.to, tc.reason, err, tc.want)
		}
	}
	if err := adapters.ActDeclared(adapterstest.New().Describe(), "open", "closed", ""); !model.IsCode(err, model.Unsupported) ||
		!strings.Contains(err.Error(), "work_item_act") {
		t.Errorf("an adapter without the capability: %v", err)
	}
}

// TestEffectVerbsNeedTheirEffect: `conversation update` and `conversation tag`
// run uma extension effects, so the adapter must ship the effect beside
// declaring extension_execute, and an adapter that declares the
// capability for another effect enables neither. `conversation tag`
// is enabled by either of the two mail effects, Gmail's labels or
// Outlook's categories, and its verb row names both when neither is
// shipped.
func TestEffectVerbsNeedTheirEffect(t *testing.T) {
	account := func(model.Channel) (string, error) { return "acct", nil }
	gh := github.NewFactory(github.Options{})
	zd := zendesk.NewFactory()
	g := google.NewFactory(google.Options{})
	m := microsoft.NewFactory(microsoft.Options{})
	adaptersOf := map[string]adapters.Adapter{
		"github":    {Factory: gh, Account: account, Extensions: gh.Extensions()},
		"zendesk":   {Factory: zd, Account: account, Extensions: zd.Extensions()},
		"google":    {Factory: g, Account: account, Extensions: g.Extensions()},
		"microsoft": {Factory: m, Account: account, Extensions: m.Extensions()},
	}
	for name, want := range map[string][]string{
		"github":    {"history fetch", "conversation status", "conversation update"},
		"zendesk":   {"conversation update"},
		"google":    {"conversation tag", "message reply-draft", "conversation unsubscribe"},
		"microsoft": {"conversation tag", "message reply-draft", "conversation unsubscribe"},
	} {
		got := adapters.VerbsOf(adaptersOf[name])
		slices.Sort(got)
		for _, w := range want {
			if !slices.Contains(got, w) {
				t.Errorf("%s enables %v, want %q among them", name, got, w)
			}
		}
	}
	for _, tc := range []struct {
		adapter string
		never   []string
	}{
		{"github", []string{"conversation tag", "message reply-draft", "conversation unsubscribe"}},
		{"zendesk", []string{"conversation tag", "message reply-draft", "conversation unsubscribe"}},
		{"google", []string{"conversation update", "conversation status"}},
		{"microsoft", []string{"conversation update", "conversation status"}},
	} {
		got := adapters.VerbsOf(adaptersOf[tc.adapter])
		for _, n := range tc.never {
			if slices.Contains(got, n) {
				t.Errorf("%s enables %q: extension_execute for an effect of its own enables only the verb that runs it", tc.adapter, n)
			}
		}
	}
	// The effects shipped beside the factory are what enables the verbs.
	for _, name := range []string{"github", "google", "microsoft"} {
		bare := adaptersOf[name]
		bare.Extensions = nil
		for _, v := range adapters.VerbsOf(bare) {
			if v == "conversation update" || v == "conversation tag" || v == "message reply-draft" || v == "conversation unsubscribe" {
				t.Errorf("%s without its effects enables %q", name, v)
			}
		}
	}
	// The verb table's reason names the effect that is missing.
	r := adapters.NewRegistry()
	for _, name := range []string{"github", "zendesk", "google"} {
		if err := r.Register(adaptersOf[name]); err != nil {
			t.Fatal(err)
		}
	}
	state := func(typ model.TypeKey, verb string) adapters.VerbState {
		t.Helper()
		for _, v := range r.VerbStates(model.Channel{Type: typ}) {
			if v.Verb == verb {
				return v
			}
		}
		t.Fatalf("no %s verb for %s", verb, typ)
		return adapters.VerbState{}
	}
	if got := state(github.Type, "conversation tag"); got.State != adapters.VerbUnsupported ||
		!strings.Contains(got.Detail, google.ExtensionLabels) || !strings.Contains(got.Detail, microsoft.ExtensionCategories) {
		t.Errorf("github.issues conversation tag: %+v, want unsupported naming both mail effects", got)
	}
	if got := state(google.Type, "conversation tag"); got.State != adapters.VerbDeclared || got.Reason != "" {
		t.Errorf("google.mail conversation tag: %+v, want declared", got)
	}
	for _, verb := range []string{"message reply-draft", "conversation unsubscribe"} {
		if got := state(google.Type, verb); got.State != adapters.VerbDeclared || got.Reason != "" {
			t.Errorf("google.mail %s: %+v, want declared", verb, got)
		}
		if got := state(zendesk.Type, verb); got.State != adapters.VerbUnsupported || got.Reason != adapters.ReasonNotDeclared ||
			!strings.Contains(got.Detail, "google.mail.") || !strings.Contains(got.Detail, "microsoft.mail.") {
			t.Errorf("zendesk.tickets %s: %+v, want unsupported naming both mail effects", verb, got)
		}
	}
	if got := state(github.Type, "conversation update"); got.State != adapters.VerbDeclared {
		t.Errorf("github.issues conversation update: %+v, want declared", got)
	}
}

// TestShipsEffect: an effect is shipped when a definition of that name
// requires the capability the verb needs.
func TestShipsEffect(t *testing.T) {
	defs := []adapter.ExtensionDefinition{
		{Name: "a.b", Version: 1, Kind: model.ExtensionKindEffect, RequiredCapability: model.CapabilityExtensionExecute},
	}
	if !adapters.ShipsEffect(defs, "a.b", model.CapabilityExtensionExecute) {
		t.Error("a shipped effect is not found")
	}
	if adapters.ShipsEffect(defs, "a.c", model.CapabilityExtensionExecute) || adapters.ShipsEffect(defs, "a.b", model.CapabilityHistory) || adapters.ShipsEffect(nil, "a.b", model.CapabilityExtensionExecute) {
		t.Error("an effect that is not shipped, or needs another capability, is found")
	}
}

// TestEffectFakesEnableTheirVerbs: the fake variants that ship an
// effect of another adapter enable the verb that runs it and no other:
// the one that ships Zendesk's update alone declares extension_execute
// and enables neither of the two new verbs.
func TestEffectFakesEnableTheirVerbs(t *testing.T) {
	for typ, want := range map[model.TypeKey][]string{
		adapterstest.UpdatesType:      {"conversation update"},
		adapterstest.LabelsType:       {"conversation tag"},
		adapterstest.CategoriesType:   {"conversation tag"},
		adapterstest.DraftsType:       {"message reply-draft"},
		adapterstest.UnsubscribeType:  {"conversation unsubscribe"},
		adapterstest.TicketEffectType: {"conversation update"},
	} {
		f := adapterstest.New()
		f.As(typ)
		switch typ {
		case adapterstest.UpdatesType:
			f.DeclareEffect(github.ExtensionUpdate)
		case adapterstest.LabelsType:
			f.DeclareEffect(google.ExtensionLabels)
		case adapterstest.CategoriesType:
			f.DeclareEffect(microsoft.ExtensionCategories)
		case adapterstest.DraftsType:
			f.DeclareEffect(google.ExtensionReplyDraft)
		case adapterstest.UnsubscribeType:
			f.DeclareEffect(google.ExtensionUnsubscribe)
		default:
			f.DeclareEffect(zendesk.ExtensionUpdate)
		}
		if got := adapters.VerbsOf(f.Adapter()); !slices.Equal(got, want) {
			t.Errorf("%s enables %v, want %v", typ, got, want)
		}
		if !adapters.Declares(f.Describe(), model.CapabilityExtensionExecute) {
			t.Errorf("%s does not declare extension_execute", typ)
		}
	}
	if adapters.Declares(adapterstest.New().Describe(), model.CapabilityExtensionExecute) {
		t.Error("the plain fake declares extension_execute")
	}
}

// TestModalVerbsNeedTheirEffect: each of the three modal verbs runs one
// Slack effect, so slack.chat enables it only with that effect shipped
// beside the factory, and an adapter declaring extension_execute for
// effects of its own enables none of them. They are not `interaction
// respond`: the modal verbs follow the effect, respond follows the
// capability of that name.
func TestModalVerbsNeedTheirEffect(t *testing.T) {
	account := func(model.Channel) (string, error) { return "acct", nil }
	sl := slack.NewFactory(slack.Options{})
	modal := []string{"interaction open-modal", "interaction push-modal", "interaction update-modal"}
	enabled := func(a adapters.Adapter) []string {
		var out []string
		for _, v := range adapters.VerbsOf(a) {
			if slices.Contains(modal, v) {
				out = append(out, v)
			}
		}
		return out
	}

	if got := enabled(adapters.Adapter{Factory: sl, Account: account, Extensions: sl.Extensions()}); !slices.Equal(got, modal) {
		t.Errorf("slack.chat with its effects enables %v, want %v", got, modal)
	}
	if got := enabled(adapters.Adapter{Factory: sl, Account: account}); len(got) != 0 {
		t.Errorf("slack.chat without its effects registered enables %v", got)
	}
	gh := github.NewFactory(github.Options{})
	if got := enabled(adapters.Adapter{Factory: gh, Account: account, Extensions: gh.Extensions()}); len(got) != 0 {
		t.Errorf("github.issues, which declares extension_execute for its own effect, enables %v", got)
	}
	// One effect, one verb.
	for _, tc := range []struct {
		effect string
		verb   string
	}{
		{slack.ExtensionViewsOpen, "interaction open-modal"},
		{slack.ExtensionViewsPush, "interaction push-modal"},
		{slack.ExtensionViewsUpdate, "interaction update-modal"},
	} {
		f := adapterstest.New()
		f.As(adapterstest.ViewsType)
		f.DeclareEffect(tc.effect)
		if got := enabled(f.Adapter()); !slices.Equal(got, []string{tc.verb}) {
			t.Errorf("a fake shipping only %s enables %v, want %s", tc.effect, got, tc.verb)
		}
	}
	// The verb table says which effect is missing.
	r := adapters.NewRegistry()
	if err := r.Register(adapters.Adapter{Factory: sl, Account: account}); err != nil {
		t.Fatal(err)
	}
	for _, v := range r.VerbStates(model.Channel{Type: slack.Type}) {
		if !slices.Contains(modal, v.Verb) {
			continue
		}
		if v.State != adapters.VerbUnsupported || v.Reason != adapters.ReasonNotDeclared || !strings.Contains(v.Detail, "slack.chat.views.") {
			t.Errorf("%s: %+v, want unsupported and naming the effect it ships none of", v.Verb, v)
		}
	}
}

// TestStatusFollowsTheDeclaredActs: gitlab.issues declares work_item_act
// with the close and the reopen, as github.issues does, so `conversation
// status` is enabled on both with no provider named; jira.issues and
// linear.issues declare no act, and zendesk.tickets none either: the verb
// is not enabled on them, and its row says the capability is missing.
func TestStatusFollowsTheDeclaredActs(t *testing.T) {
	account := func(model.Channel) (string, error) { return "acct", nil }
	for typ, f := range map[model.TypeKey]adapter.Factory{
		model.TypeGitlabIssues: gitlab.NewFactory(gitlab.Options{}),
		model.TypeJiraIssues:   jira.NewFactory(),
		model.TypeLinearIssues: linear.NewFactory(linear.Options{}),
		zendesk.Type:           zendesk.NewFactory(),
		github.Type:            github.NewFactory(github.Options{}),
	} {
		got := adapters.VerbsOf(adapters.Adapter{Factory: f, Account: account})
		want := adapters.Declares(f.Describe(), model.CapabilityWorkItemAct)
		if has := slices.Contains(got, "conversation status"); has != want || want != (typ == github.Type || typ == model.TypeGitlabIssues) {
			t.Errorf("%s enables %v: conversation status enabled = %v, declares the act = %v", typ, got, has, want)
		}
	}
	r := adapters.NewRegistry()
	if err := r.Register(adapters.Adapter{Factory: jira.NewFactory(), Account: account}); err != nil {
		t.Fatal(err)
	}
	for _, v := range r.VerbStates(model.Channel{Type: model.TypeJiraIssues}) {
		if v.Verb == "conversation status" && (v.State != adapters.VerbUnsupported || v.Reason != adapters.ReasonNotDeclared) {
			t.Errorf("jira.issues conversation status: %+v", v)
		}
	}
}

// TestStateVerbsFollowTheirCapability: each pair of conversation state
// verbs is enabled by the one capability that carries it and by nothing
// else; a fake declaring one capability enables only its verbs, and
// extension_execute enables none of them.
func TestStateVerbsFollowTheirCapability(t *testing.T) {
	for c, want := range map[model.Capability][]string{
		model.CapabilityArchive:   {"conversation archive", "conversation unarchive"},
		model.CapabilityMove:      {"conversation move"},
		model.CapabilityReadState: {"conversation read", "conversation unread"},
		model.CapabilityFlag:      {"conversation flag", "conversation unflag"},
		model.CapabilityTrash:     {"conversation trash", "conversation untrash"},
	} {
		if got := adapters.VerbsFor(c); !slices.Equal(got, want) {
			t.Errorf("%s enables %v, want %v", c, got, want)
		}
		f := adapterstest.New()
		f.DeclareState(adapterstest.StateOptions{Caps: []model.Capability{c}})
		if got := adapters.VerbsOf(f.Adapter()); !slices.Equal(got, want) {
			t.Errorf("a fake declaring only %s enables %v, want %v", c, got, want)
		}
	}
	all := adapterstest.New()
	all.DeclareState(adapterstest.StateOptions{})
	if got := adapters.VerbsOf(all.Adapter()); len(got) != 9 {
		t.Errorf("a fake declaring every state capability enables %v, want the nine state verbs", got)
	}
	for _, v := range adapters.VerbsFor(model.CapabilityExtensionExecute) {
		if strings.HasPrefix(v, "conversation ") && slices.Contains([]string{"archive", "move", "read", "flag", "trash"}, strings.Fields(v)[1]) {
			t.Errorf("extension_execute enables the state verb %q", v)
		}
	}
	// The mail adapters declare all five, slack and discord archive only.
	for typ, want := range map[model.TypeKey]int{google.Type: 9, microsoft.Type: 9, slack.Type: 2} {
		var n int
		for _, tv := range []model.TypeKey{typ} {
			a := stateAdapter(t, tv)
			for _, v := range adapters.VerbsOf(a) {
				if slices.Contains([]string{"conversation archive", "conversation unarchive", "conversation move", "conversation read",
					"conversation unread", "conversation flag", "conversation unflag", "conversation trash", "conversation untrash"}, v) {
					n++
				}
			}
		}
		if n != want {
			t.Errorf("%s enables %d state verbs, want %d", typ, n, want)
		}
	}
}

// stateAdapter is the maintained adapter of type typ, as heo registers it.
func stateAdapter(t *testing.T, typ model.TypeKey) adapters.Adapter {
	t.Helper()
	switch typ {
	case google.Type:
		g := google.NewFactory(google.Options{})
		return adapters.Adapter{Factory: g, Extensions: g.Extensions()}
	case microsoft.Type:
		m := microsoft.NewFactory(microsoft.Options{})
		return adapters.Adapter{Factory: m, Extensions: m.Extensions()}
	case slack.Type:
		s := slack.NewFactory(slack.Options{})
		return adapters.Adapter{Factory: s, Extensions: s.Extensions()}
	}
	t.Fatalf("no adapter %s", typ)
	return adapters.Adapter{}
}
