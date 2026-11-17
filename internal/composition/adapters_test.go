package composition_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/composition"
	"hop.top/kit/go/core/netpolicy"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/chat/discord"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/adapter/chat/teams"
	"hop.top/uma/adapter/chat/telegram"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/adapter/mail/microsoft"
	"hop.top/uma/adapter/mail/smtp"
	metawhatsapp "hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/adapter/messaging/twilio/sms"
	twiliowhatsapp "hop.top/uma/adapter/messaging/twilio/whatsapp"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/adapter/ticket/gitlab"
	"hop.top/uma/adapter/ticket/jira"
	"hop.top/uma/adapter/ticket/linear"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/memory"
)

func settings(kv ...any) *viper.Viper {
	v := viper.New()
	for k, val := range composition.Defaults() {
		v.SetDefault(k, val)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i].(string), kv[i+1])
	}
	return v
}

func registry(t *testing.T, v *viper.Viper) *adapters.Registry {
	t.Helper()
	r := adapters.NewRegistry()
	if err := composition.Register(r, composition.Options{Viper: func() *viper.Viper { return v }}); err != nil {
		t.Fatal(err)
	}
	return r
}

func ext(t *testing.T, schema string, v any) model.Extension {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return model.Extension{Name: schema, Version: 1, Data: data}
}

// TestRegisterAdapters: the composition root registers every
// direct-message adapter and both ticket adapters, each receiving one
// naming its provider account, and uma accepts their descriptors and
// the ticket adapters' extension effects.
func TestRegisterAdapters(t *testing.T) {
	r := registry(t, settings())
	want := []model.TypeKey{
		model.TypeNativeWebWidget, model.TypeSmtpMail, model.TypeImapMail, model.TypeGoogleMail,
		model.TypeMicrosoftMail, model.TypeTwilioSms, model.TypeTwilioWhatsapp, model.TypeMetaWhatsapp,
		model.TypeRelayMessages,
		model.TypeDiscordChat, model.TypeSlackChat, model.TypeTelegramMessages, model.TypeMicrosoftTeams, model.TypeGithubIssues, model.TypeZendeskTickets,
		model.TypeGitlabIssues, model.TypeJiraIssues, model.TypeLinearIssues,
	}
	if got := r.Types(); !slices.Equal(got, want) {
		t.Fatalf("registered %v, want %v", got, want)
	}
	for _, typ := range want {
		a, _ := r.Lookup(typ)
		if adapters.Receives(a.Factory.Describe()) && a.Account == nil {
			t.Errorf("%s receives but names no provider account", typ)
		}
	}
	if a, _ := r.Lookup(widget.Type); a.Routes == nil {
		t.Error("widget registered without its event stream route")
	}
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0))
	e, err := uma.New(uma.Config{
		Store: memory.New(memory.Options{Clock: clock}),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{}, nil
		}),
		Adapters: r.Factories(), Extensions: r.Extensions(), Clock: clock, IDs: portstest.NewSequenceIDs(1),
	})
	if err != nil {
		t.Fatalf("uma refuses the adapters: %v", err)
	}
	_ = e.Close(context.Background())
}

// TestExtensionEffects: the extension effects heo surfaces are
// registered beside the adapters that ship them, from uma's own
// definitions: the mail adapters' mailbox list and create, Gmail's labels, Outlook's categories, GitHub's issue
// update and Zendesk's ticket update and internal note, and Slack's
// three modal effects, each authorized as the policy action uma's
// definition names; no other adapter ships any.
func TestExtensionEffects(t *testing.T) {
	r := registry(t, settings())
	want := slices.Concat(imap.NewFactory(imap.Options{}).Extensions(), google.NewFactory(google.Options{}).Extensions(), microsoft.NewFactory(microsoft.Options{}).Extensions(),
		slack.NewFactory(slack.Options{}).Extensions(), github.NewFactory(github.Options{}).Extensions(), zendesk.NewFactory().Extensions())
	if got := r.Extensions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("extensions = %+v, want %+v", got, want)
	}
	for _, tc := range []struct {
		typ    model.TypeKey
		effect string
		action string
	}{
		{zendesk.Type, zendesk.ExtensionUpdate, model.ActionWorkItemAct},
		{github.Type, github.ExtensionUpdate, model.ActionWorkItemAct},
		{google.Type, google.ExtensionLabels, model.ActionConversationLabel},
		{microsoft.Type, microsoft.ExtensionCategories, model.ActionConversationLabel},
		{slack.Type, slack.ExtensionViewsOpen, model.ActionExtensionExecute},
		{slack.Type, slack.ExtensionViewsPush, model.ActionExtensionExecute},
		{slack.Type, slack.ExtensionViewsUpdate, model.ActionExtensionExecute},
	} {
		a, _ := r.Lookup(tc.typ)
		if !slices.ContainsFunc(a.Extensions, func(d adapter.ExtensionDefinition) bool {
			return d.Name == tc.effect && d.AuthorizeAs == tc.action
		}) {
			t.Errorf("%s extensions lack %s authorized as %s: %+v", tc.typ, tc.effect, tc.action, a.Extensions)
		}
	}
}

// channels is one valid channel configuration per adapter.
func channels(t *testing.T) map[model.TypeKey]model.Channel {
	t.Helper()
	ref := model.ChannelRef{Tenant: "acme", ID: "00000000-0000-7000-8000-000000000001"}
	ch := func(typ model.TypeKey, cfg model.Extension) model.Channel {
		return model.Channel{Ref: ref, Type: typ, Configuration: cfg, CredentialRef: "cred"}
	}
	return map[model.TypeKey]model.Channel{
		widget.Type: ch(widget.Type, widget.Configuration("site-1")),
		model.TypeSmtpMail: ch(model.TypeSmtpMail, ext(t, smtp.ConfigSchema, smtp.Config{
			Hostname: "mx.example.com", From: "support@example.com",
			Recipients: []smtp.Recipient{{Address: "support@example.com", Destination: "support"}},
			Relay:      smtp.Relay{Host: "relay.example.com", TLSPort: 465},
		})),
		imap.Type: ch(imap.Type, ext(t, imap.ConfigSchema, imap.Config{
			Address: "support@example.com", IMAP: imap.Server{Host: "IMAP.example.com", TLSPort: 993},
			Submission: imap.Server{Host: "smtp.example.com", TLSPort: 465},
		})),
		google.Type:    ch(google.Type, ext(t, google.ConfigSchema, google.Config{User: "Support@Example.com"})),
		microsoft.Type: ch(microsoft.Type, ext(t, microsoft.ConfigSchema, microsoft.Config{Mailbox: "Support@Example.com"})),
		model.TypeTwilioSms: ch(model.TypeTwilioSms, sms.Config{
			AccountSID: "AC00000000000000000000000000000001", From: "+15005550006",
		}.Extension()),
		model.TypeTwilioWhatsapp: ch(model.TypeTwilioWhatsapp, twiliowhatsapp.Config{
			AccountSID: "AC00000000000000000000000000000001", MessagingServiceSID: "MG00000000000000000000000000000001",
		}.Extension()),
		model.TypeMetaWhatsapp: ch(model.TypeMetaWhatsapp, metawhatsapp.Config{PhoneNumberID: "100000000000001"}.Extension()),
		model.TypeRelayMessages: ch(model.TypeRelayMessages, relay.Config{Medium: relay.MediumIMessage, Auth: relay.Auth{Scheme: "bearer"},
			Callback: &relay.Callback{URL: "https://gw.example.com:8787/uma/reply"}}.Extension()),
		discord.Type:  ch(discord.Type, discord.Config{ApplicationID: "900000000000000001", BotUserID: "900000000000000002"}.Extension()),
		slack.Type:    ch(slack.Type, slack.Config{TeamID: "T0000000001", BotUserID: "U0000000001"}.Extension()),
		telegram.Type: ch(telegram.Type, telegram.Config{BotID: 7000000001, BotUsername: "heo_test_bot"}.Extension()),
		teams.Type:    ch(teams.Type, teamsConfig.Extension()),
		github.Type: ch(github.Type, github.Config{
			APIURL: "https://GHE.example.com/api/v3/", Replies: true, AccountID: 5000001,
		}.Extension()),
		zendesk.Type: ch(zendesk.Type, zendesk.Config{
			Subdomain: "acme", Auth: zendesk.AuthAPIToken, Email: "agent@example.com", WebhookToleranceSeconds: 300,
		}.Extension()),
		gitlab.Type: ch(gitlab.Type, gitlab.Config{
			APIURL: "https://GitLab.example.com/api/v4/", Replies: true, AccountID: 6000001,
		}.Extension()),
		jira.Type: ch(jira.Type, jira.Config{
			SiteURL: "https://Acme.atlassian.net", Replies: true, AccountID: "5b10ac8d82e05b22cc7d4ef5", Email: "agent@example.com",
		}.Extension()),
		linear.Type: ch(linear.Type, linear.Config{
			Replies: true, AccountID: "8f1b7a30-0000-4000-8000-000000000001",
		}.Extension()),
	}
}

// TestChannelConfigurationValidated: each adapter's valid configuration
// passes its validation through the registry, and a configuration of
// the wrong schema does not.
func TestChannelConfigurationValidated(t *testing.T) {
	r := registry(t, settings(composition.KeyRelayCallbackHosts, "gw.example.com:8787"))
	for typ, ch := range channels(t) {
		a, ok := r.Lookup(typ)
		if !ok {
			t.Fatalf("%s not registered", typ)
		}
		if issues := a.Factory.Validate(ch.Configuration); len(issues) > 0 {
			t.Errorf("%s: valid configuration refused: %+v", typ, issues)
		}
		if issues := a.Factory.Validate(model.Extension{Name: "other.config", Version: 1, Data: []byte("{}")}); len(issues) == 0 {
			t.Errorf("%s: foreign configuration accepted", typ)
		}
	}
}

// TestProviderAccounts: what each adapter's channel is keyed on for the
// one-receiver-per-account checks.
func TestProviderAccounts(t *testing.T) {
	r := registry(t, settings(composition.KeySMTPAddr, "127.0.0.1:2525"))
	want := map[model.TypeKey]string{
		widget.Type:              "channel:00000000-0000-7000-8000-000000000001",
		model.TypeSmtpMail:       "listen:127.0.0.1:2525",
		imap.Type:                "imap.example.com/support@example.com/INBOX",
		google.Type:              "user:support@example.com",
		microsoft.Type:           "global/mailbox:support@example.com",
		model.TypeTwilioSms:      "AC00000000000000000000000000000001/from:+15005550006",
		model.TypeTwilioWhatsapp: "AC00000000000000000000000000000001/service:MG00000000000000000000000000000001",
		model.TypeMetaWhatsapp:   "phone:100000000000001",
		model.TypeRelayMessages:  "imessage/gw.example.com:8787/uma/reply",
		discord.Type:             "discord.com/application:900000000000000001",
		slack.Type:               "slack.com/team:T0000000001/bot:U0000000001",
		telegram.Type:            "api.telegram.org/bot:7000000001",
		teams.Type:               "tenant:" + teamsTenant + "/app:" + teamsApp,
		github.Type:              "ghe.example.com/account:5000001",
		zendesk.Type:             "subdomain:acme",
		gitlab.Type:              "gitlab.example.com/account:6000001",
		jira.Type:                "acme.atlassian.net/account:5b10ac8d82e05b22cc7d4ef5",
		linear.Type:              "api.linear.app/account:8f1b7a30-0000-4000-8000-000000000001",
	}
	for typ, ch := range channels(t) {
		a, _ := r.Lookup(typ)
		got, err := a.AccountOf(ch)
		if err != nil || got != string(typ)+"\x00"+want[typ] {
			t.Errorf("%s account = %q, %v; want %q", typ, got, err, want[typ])
		}
	}
}

// TestProviderAccountFallbacks: a mailbox addressed as the signed-in
// user is keyed on its credential reference; a channel naming nothing
// to key on, or an smtp.mail channel without a listener, has no
// account.
func TestProviderAccountFallbacks(t *testing.T) {
	r := registry(t, settings())
	account := func(typ model.TypeKey, ch model.Channel) (string, error) {
		a, _ := r.Lookup(typ)
		return a.AccountOf(ch)
	}
	me := model.Channel{Type: google.Type, CredentialRef: "gmail-ops", Configuration: ext(t, google.ConfigSchema, google.Config{})}
	if got, err := account(google.Type, me); err != nil || !strings.HasSuffix(got, "\x00credential:gmail-ops") {
		t.Errorf("gmail as me = %q, %v", got, err)
	}
	me = model.Channel{Type: microsoft.Type, CredentialRef: "graph-ops", Configuration: ext(t, microsoft.ConfigSchema, microsoft.Config{Cloud: "usgov"})}
	if got, err := account(microsoft.Type, me); err != nil || !strings.HasSuffix(got, "\x00usgov/credential:graph-ops") {
		t.Errorf("graph as me = %q, %v", got, err)
	}
	me.CredentialRef = ""
	if _, err := account(microsoft.Type, me); err == nil {
		t.Error("graph mailbox without mailbox or credential got an account")
	}
	if _, err := account(model.TypeSmtpMail, channels(t)[model.TypeSmtpMail]); err == nil ||
		!strings.Contains(err.Error(), composition.KeySMTPAddr+" is not set") {
		t.Errorf("smtp without a listener: %v", err)
	}
	if _, err := account(model.TypeMetaWhatsapp, model.Channel{Configuration: ext(t, sms.ConfigSchema, map[string]any{})}); err == nil {
		t.Error("foreign configuration got an account")
	}
	// A GitHub channel without the account ID its credential acts as
	// (webhooks only) is keyed on its credential reference, on github.com
	// by default.
	gh := model.Channel{Type: github.Type, CredentialRef: "gh-ops", Configuration: github.Config{}.Extension()}
	if got, err := account(github.Type, gh); err != nil || !strings.HasSuffix(got, "\x00api.github.com/credential:gh-ops") {
		t.Errorf("github without account ID = %q, %v", got, err)
	}
	gh.CredentialRef = ""
	if _, err := account(github.Type, gh); err == nil {
		t.Error("github channel without account ID or credential got an account")
	}
	if _, err := account(zendesk.Type, model.Channel{Type: zendesk.Type, Configuration: zendesk.Config{}.Extension()}); err == nil {
		t.Error("zendesk channel without subdomain got an account")
	}
	// GitLab, Jira and Linear without the account ID their credential
	// acts as (webhooks only) are keyed on the credential reference, on
	// the instance, site or API host the configuration names (gitlab.com
	// and Linear's API by default); none without either.
	for name, c := range map[string]struct {
		ch   model.Channel
		want string
	}{
		"gitlab": {model.Channel{Type: gitlab.Type, CredentialRef: "gl-ops", Configuration: gitlab.Config{}.Extension()}, "gitlab.com/credential:gl-ops"},
		"jira": {model.Channel{Type: jira.Type, CredentialRef: "jira-ops", Configuration: jira.Config{SiteURL: "https://acme.atlassian.net"}.Extension()},
			"acme.atlassian.net/credential:jira-ops"},
		"linear": {model.Channel{Type: linear.Type, CredentialRef: "lin-ops", Configuration: linear.Config{}.Extension()}, "api.linear.app/credential:lin-ops"},
	} {
		if got, err := account(c.ch.Type, c.ch); err != nil || !strings.HasSuffix(got, "\x00"+c.want) {
			t.Errorf("%s without account ID = %q, %v", name, got, err)
		}
		c.ch.CredentialRef = ""
		if _, err := account(c.ch.Type, c.ch); err == nil {
			t.Errorf("%s channel without account ID or credential got an account", name)
		}
	}
	if _, err := account(jira.Type, model.Channel{Type: jira.Type, CredentialRef: "j", Configuration: jira.Config{}.Extension()}); err == nil {
		t.Error("jira channel without a site got an account")
	}
	// Another host or another account is another account.
	other, _ := account(gitlab.Type, model.Channel{Type: gitlab.Type, Configuration: gitlab.Config{AccountID: 6000001}.Extension()})
	if same, _ := account(gitlab.Type, model.Channel{Type: gitlab.Type, Configuration: gitlab.Config{AccountID: 6000001, APIURL: "https://gitlab.example.com/api/v4/"}.Extension()}); same == other {
		t.Error("gitlab.com and a self-managed host are one account")
	}
}

// TestIssueRepliesOff: each issue adapter reports send, edit and delete
// (and, for jira.issues, reconcile) unavailable until the channel's
// replies key is on; with replies on, or a configuration that does not
// parse, nothing is reported off.
func TestIssueRepliesOff(t *testing.T) {
	r := registry(t, settings())
	for typ, ext := range map[model.TypeKey][2]model.Extension{
		gitlab.Type: {gitlab.Config{}.Extension(), gitlab.Config{Replies: true, AccountID: 1}.Extension()},
		jira.Type:   {jira.Config{SiteURL: "https://a.atlassian.net"}.Extension(), jira.Config{SiteURL: "https://a.atlassian.net", Replies: true, AccountID: "x"}.Extension()},
		linear.Type: {linear.Config{}.Extension(), linear.Config{Replies: true, AccountID: "x"}.Extension()},
	} {
		a, _ := r.Lookup(typ)
		off := a.CapabilityOff(model.Channel{Type: typ, Configuration: ext[0]})
		for _, c := range []model.Capability{model.CapabilitySend, model.CapabilityEdit, model.CapabilityDelete} {
			if off[c] == "" {
				t.Errorf("%s with replies off does not report %s off: %v", typ, c, off)
			}
		}
		if _, rec := off[model.CapabilityReconcile]; rec != (typ == jira.Type) {
			t.Errorf("%s reconcile off = %v", typ, rec)
		}
		if on := a.CapabilityOff(model.Channel{Type: typ, Configuration: ext[1]}); len(on) != 0 {
			t.Errorf("%s with replies on reports %v off", typ, on)
		}
		if bad := a.CapabilityOff(model.Channel{Type: typ, Configuration: model.Extension{Name: "other.config", Version: 1, Data: []byte("{}")}}); len(bad) != 0 {
			t.Errorf("%s with a foreign configuration reports %v off", typ, bad)
		}
	}
}

// TestSlackAccount: a slack.chat channel's account is the bot user of
// the app installed in one workspace, on the Web API host it calls: a
// different workspace, bot or host is another account, the host's case
// is not, and a channel naming no workspace or bot has none.
func TestSlackAccount(t *testing.T) {
	r := registry(t, settings())
	a, _ := r.Lookup(slack.Type)
	account := func(cfg slack.Config) (string, error) {
		return a.AccountOf(model.Channel{Type: slack.Type, CredentialRef: "slack", Configuration: cfg.Extension()})
	}
	base := slack.Config{TeamID: "T0000000001", BotUserID: "U0000000001"}
	first, err := account(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]slack.Config{
		"another workspace": {TeamID: "T0000000002", BotUserID: "U0000000001"},
		"another bot":       {TeamID: "T0000000001", BotUserID: "U0000000002"},
		"another host":      {TeamID: "T0000000001", BotUserID: "U0000000001", APIURL: "https://slack.example.com/api/"},
	} {
		if got, err := account(cfg); err != nil || got == first {
			t.Errorf("%s = %q, %v; want an account other than %q", name, got, err, first)
		}
	}
	// Option and optional fields that do not name the receiver leave it
	// the same.
	same := base
	same.BotID, same.AppID, same.MentionsOnly, same.APIURL = "B0000000001", "A0000000001", true, "https://SLACK.com/api"
	if got, err := account(same); err != nil || got != first {
		t.Errorf("same install with optional fields = %q, %v; want %q", got, err, first)
	}
	for name, cfg := range map[string]slack.Config{
		"no workspace": {BotUserID: "U0000000001"},
		"no bot user":  {TeamID: "T0000000001"},
	} {
		if got, err := account(cfg); err == nil {
			t.Errorf("%s got account %q", name, got)
		}
	}
	if _, err := a.AccountOf(model.Channel{Type: slack.Type, Configuration: ext(t, sms.ConfigSchema, map[string]any{})}); err == nil {
		t.Error("foreign configuration got an account")
	}
}

// TestDiscordAccount: a discord.chat channel's account is its application,
// on the REST host it calls: one bot token, one gateway session and one
// Interactions Endpoint URL per application, however many guilds the bot is
// in. Another application or host is another account; the host's case and
// the options that do not name the application are not; a channel naming
// no application has none. The account does not depend on how the channel
// receives, so a gateway channel and an interactions channel of one
// application share it.
func TestDiscordAccount(t *testing.T) {
	r := registry(t, settings())
	a, _ := r.Lookup(discord.Type)
	account := func(cfg discord.Config) (string, error) {
		return a.AccountOf(model.Channel{Type: discord.Type, CredentialRef: "dc", Configuration: cfg.Extension()})
	}
	base := discord.Config{ApplicationID: "900000000000000001", BotUserID: "900000000000000002"}
	first, err := account(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]discord.Config{
		"another application": {ApplicationID: "900000000000000003", BotUserID: "900000000000000002"},
		"another host":        {ApplicationID: "900000000000000001", BotUserID: "900000000000000002", APIURL: "https://discord.example.com/api/v10/"},
	} {
		if got, err := account(cfg); err != nil || got == first {
			t.Errorf("%s = %q, %v; want an account other than %q", name, got, err, first)
		}
	}
	no := false
	same := base
	same.BotUserID, same.MentionsOnly, same.Receive, same.APIURL = "900000000000000009", &no, discord.ReceiveInteractions, "https://DISCORD.com/api/v10"
	if got, err := account(same); err != nil || got != first {
		t.Errorf("same application with other options = %q, %v; want %q", got, err, first)
	}
	if got, err := account(discord.Config{BotUserID: "900000000000000002"}); err == nil {
		t.Errorf("no application ID got account %q", got)
	}
	if _, err := a.AccountOf(model.Channel{Type: discord.Type, Configuration: ext(t, sms.ConfigSchema, map[string]any{})}); err == nil {
		t.Error("foreign configuration got an account")
	}
}

// TestTelegramAccount: a telegram.messages channel's account is its bot,
// on the Bot API host it calls: another bot or host is another account,
// the host's case and the options that do not name the bot are not, and a
// channel naming no bot has none. The account does not depend on how the
// channel receives, so two channels of one bot (one by webhook, one by
// polling) share it.
func TestTelegramAccount(t *testing.T) {
	r := registry(t, settings())
	a, _ := r.Lookup(telegram.Type)
	account := func(cfg telegram.Config) (string, error) {
		return a.AccountOf(model.Channel{Type: telegram.Type, CredentialRef: "tg", Configuration: cfg.Extension()})
	}
	base := telegram.Config{BotID: 7000000001, BotUsername: "heo_test_bot"}
	first, err := account(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]telegram.Config{
		"another bot":  {BotID: 7000000002, BotUsername: "heo_test_bot"},
		"another host": {BotID: 7000000001, BotUsername: "heo_test_bot", APIURL: "https://telegram.example.com/"},
	} {
		if got, err := account(cfg); err != nil || got == first {
			t.Errorf("%s = %q, %v; want an account other than %q", name, got, err, first)
		}
	}
	same := base
	same.BotUsername, same.MentionsOnly, same.PollTimeoutSeconds, same.APIURL = "other_bot", true, 50, "https://API.telegram.org"
	if got, err := account(same); err != nil || got != first {
		t.Errorf("same bot with other options = %q, %v; want %q", got, err, first)
	}
	if got, err := account(telegram.Config{BotUsername: "heo_test_bot"}); err == nil {
		t.Errorf("no bot ID got account %q", got)
	}
	if _, err := a.AccountOf(model.Channel{Type: telegram.Type, Configuration: ext(t, sms.ConfigSchema, map[string]any{})}); err == nil {
		t.Error("foreign configuration got an account")
	}
}

// The one tenant and bot registration the microsoft.teams fixtures name.
const (
	teamsTenant = "1111aaaa-1111-4111-8111-11111111bbbb"
	teamsApp    = "2222cccc-2222-4222-8222-22222222dddd"
)

var teamsConfig = teams.Config{AppID: teamsApp, TenantID: teamsTenant, ServiceURL: "https://smba.trafficmanager.net/teams/"}

// TestTeamsAccount: a microsoft.teams channel's account is its bot
// registration, the App ID, in its Entra tenant: another App ID or tenant
// is another account, the GUIDs' case and the service URL (where the bot
// sends, not what it receives) are not, and a channel naming no bot or
// tenant has none. The Bot Connector posts every activity of a
// registration to its one messaging endpoint, so two channels of one bot
// would be handed the same activities.
func TestTeamsAccount(t *testing.T) {
	r := registry(t, settings())
	a, _ := r.Lookup(teams.Type)
	account := func(cfg teams.Config) (string, error) {
		return a.AccountOf(model.Channel{Type: teams.Type, CredentialRef: "teams", Configuration: cfg.Extension()})
	}
	first, err := account(teamsConfig)
	if err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]teams.Config{
		"another app":    {AppID: "33333333-3333-4333-8333-333333333333", TenantID: teamsTenant, ServiceURL: teamsConfig.ServiceURL},
		"another tenant": {AppID: teamsApp, TenantID: "44444444-4444-4444-8444-444444444444", ServiceURL: teamsConfig.ServiceURL},
	} {
		if got, err := account(cfg); err != nil || got == first {
			t.Errorf("%s = %q, %v; want an account other than %q", name, got, err, first)
		}
	}
	same := teams.Config{
		AppID: strings.ToUpper(teamsApp), TenantID: strings.ToUpper(teamsTenant), ServiceURL: "https://smba.infra.gcc.teams.microsoft.com/teams",
	}
	if got, err := account(same); err != nil || got != first {
		t.Errorf("same bot, other case and service URL = %q, %v; want %q", got, err, first)
	}
	for name, cfg := range map[string]teams.Config{
		"no app":    {TenantID: teamsTenant, ServiceURL: teamsConfig.ServiceURL},
		"no tenant": {AppID: teamsApp, ServiceURL: teamsConfig.ServiceURL},
	} {
		if got, err := account(cfg); err == nil {
			t.Errorf("%s got account %q", name, got)
		}
	}
	if _, err := a.AccountOf(model.Channel{Type: teams.Type, Configuration: ext(t, sms.ConfigSchema, map[string]any{})}); err == nil {
		t.Error("foreign configuration got an account")
	}
}

// TestSMTPSettingsChecked: the listener address must be host:port, on
// loopback unless allow_remote; a remote listener without STARTTLS is
// refused unless allow_remote_without_tls accepts plain text, and then
// warned about.
func TestSMTPSettingsChecked(t *testing.T) {
	for _, tc := range []struct {
		addr      string
		remote    bool
		plaintext bool
		wantKey   string
		want      string
		warning   bool
	}{
		{addr: ""},
		{addr: "127.0.0.1:25"},
		{addr: "localhost:2525"},
		{addr: "0.0.0.0:25", wantKey: composition.KeySMTPAddr, want: "not a loopback address"},
		{addr: "0.0.0.0:25", remote: true, wantKey: composition.KeySMTPAllowRemote, want: "without STARTTLS"},
		{addr: "0.0.0.0:25", remote: true, plaintext: true, wantKey: composition.KeySMTPAllowRemoteWithoutTLS, want: "plain text", warning: true},
		{addr: "mx.example.com", wantKey: composition.KeySMTPAddr, want: "want host:port"},
	} {
		v := settings(composition.KeySMTPAddr, tc.addr, composition.KeySMTPAllowRemote, tc.remote,
			composition.KeySMTPAllowRemoteWithoutTLS, tc.plaintext)
		probs := composition.Check(context.Background(), v, nil)
		switch {
		case tc.want == "" && len(probs) > 0:
			t.Errorf("%q remote=%v: %+v", tc.addr, tc.remote, probs)
		case tc.want != "" && (len(probs) != 1 || probs[0].Key != tc.wantKey || !strings.Contains(probs[0].Message, tc.want) || probs[0].Warning != tc.warning):
			t.Errorf("%q remote=%v plaintext=%v: %+v, want %q on %s (warning %v)", tc.addr, tc.remote, tc.plaintext, probs, tc.want, tc.wantKey, tc.warning)
		}
	}
	// The receiver refuses the listener the check refuses.
	v := settings(composition.KeySMTPAddr, "0.0.0.0:25", composition.KeySMTPAllowRemote, true)
	a, _ := registry(t, v).Lookup(model.TypeSmtpMail)
	if _, err := a.AccountOf(channels(t)[model.TypeSmtpMail]); err == nil || !strings.Contains(err.Error(), "without STARTTLS") {
		t.Errorf("remote listener without TLS has an account: %v", err)
	}
}

// TestWidgetSettingsChecked: adapters.widget.retain defaults to uma's
// retention and must keep at least one entry; composing the adapters over
// a store refuses what the check refuses.
func TestWidgetSettingsChecked(t *testing.T) {
	cfg, probs := composition.ReadWidget(settings())
	if cfg.Retain != widget.DefaultRetain || len(probs) > 0 {
		t.Errorf("defaults = %+v, %+v; want retain %d", cfg, probs, widget.DefaultRetain)
	}
	if cfg, probs := composition.ReadWidget(settings(composition.KeyWidgetRetain, 3)); cfg.Retain != 3 || len(probs) > 0 {
		t.Errorf("retain 3 = %+v, %+v", cfg, probs)
	}
	for _, bad := range []any{0, -1, "many"} {
		probs := composition.Check(context.Background(), settings(composition.KeyWidgetRetain, bad), nil)
		if len(probs) != 1 || probs[0].Key != composition.KeyWidgetRetain || probs[0].Warning ||
			!strings.Contains(probs[0].Message, "at least 1") {
			t.Errorf("retain %v: %+v", bad, probs)
		}
		v := settings(composition.KeyWidgetRetain, bad)
		_, err := composition.Adapters(composition.Options{Viper: func() *viper.Viper { return v }, Store: memory.New(memory.Options{})})
		if err == nil || !strings.Contains(err.Error(), composition.KeyWidgetRetain) {
			t.Errorf("adapters over a store with retain %v: %v", bad, err)
		}
	}
}

// TestHTTPClientHonorsOfflinePolicy: provider traffic is refused inside
// heo's client on an offline run, before anything is dialed.
func TestHTTPClientHonorsOfflinePolicy(t *testing.T) {
	var dialed bool
	base := roundTripper(func(*http.Request) (*http.Response, error) {
		dialed = true
		return nil, errors.New("unexpected dial")
	})
	c := composition.NewHTTPClient(base)
	req, err := http.NewRequestWithContext(netpolicy.WithOffline(context.Background(), true), http.MethodGet, "https://api.twilio.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	if err == nil || dialed {
		t.Fatalf("offline request = %v, dialed %v; want refused before the transport", err, dialed)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestHTTPClientUnderInstalledPolicy: kit's root guards
// http.DefaultTransport process wide; heo's client is built all the
// same.
func TestHTTPClientUnderInstalledPolicy(t *testing.T) {
	prev := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = prev })
	netpolicy.Install()
	if composition.NewHTTPClient(nil) == nil {
		t.Fatal("no client")
	}
}

// TestCredentialPurposes: each adapter names the purposes its channels'
// credential reference resolves for — what `channel check --remote`
// diagnoses — using uma's own purpose constants.
func TestCredentialPurposes(t *testing.T) {
	r := registry(t, settings())
	want := map[model.TypeKey][]string{
		widget.Type:              {widget.CredentialPurpose},
		model.TypeSmtpMail:       {smtp.PurposeRelay},
		imap.Type:                {imap.PurposeIMAP, imap.PurposeSubmission},
		google.Type:              {google.PurposeGmail},
		microsoft.Type:           {microsoft.PurposeGraph, microsoft.PurposeNotifications},
		model.TypeTwilioSms:      {sms.PurposeREST, sms.PurposeWebhook},
		model.TypeTwilioWhatsapp: {twiliowhatsapp.PurposeREST, twiliowhatsapp.PurposeWebhook},
		model.TypeMetaWhatsapp:   {metawhatsapp.PurposeGraph, metawhatsapp.PurposeAppSecret, metawhatsapp.PurposeVerifyToken},
		model.TypeRelayMessages:  {relay.PurposeWebhook, relay.PurposeCallback},
		discord.Type:             {discord.CredentialPurposeAPI, discord.CredentialPurposeInteractions},
		slack.Type:               {slack.CredentialPurposeAPI, slack.CredentialPurposeSigning, slack.CredentialPurposeSocket},
		telegram.Type:            {telegram.CredentialPurposeAPI, telegram.CredentialPurposeWebhook},
		teams.Type:               {composition.PurposeTeamsClientSecret, composition.PurposeTeamsClientCertificate},
		github.Type:              {github.CredentialPurposeAPI, github.CredentialPurposeWebhook},
		zendesk.Type:             {zendesk.CredentialPurposeAPI, zendesk.CredentialPurposeWebhook},
		gitlab.Type:              {gitlab.CredentialPurposeAPI, gitlab.CredentialPurposeWebhook},
		jira.Type:                {jira.CredentialPurposeAPI, jira.CredentialPurposeWebhook},
		linear.Type:              {linear.CredentialPurposeAPI, linear.CredentialPurposeWebhook},
	}
	// Purposes only one way of receiving resolves: needed unless the
	// channel's configuration switches that way off.
	receiving := map[model.TypeKey]map[string]model.Capability{
		discord.Type: {discord.CredentialPurposeInteractions: model.CapabilityReceiveHTTP},
		slack.Type: {
			slack.CredentialPurposeSigning: model.CapabilityReceiveHTTP,
			slack.CredentialPurposeSocket:  model.CapabilityReceive,
		},
		telegram.Type: {telegram.CredentialPurposeWebhook: model.CapabilityReceiveHTTP},
	}
	// Purposes only sending resolves: needed unless the channel's
	// configuration switches send off (a relay with no callback).
	sending := map[model.TypeKey][]string{model.TypeRelayMessages: {relay.PurposeCallback}}
	for _, typ := range r.Types() {
		a, _ := r.Lookup(typ)
		if got := a.CredentialPurposes; !slices.Equal(got, want[typ]) {
			t.Errorf("%s purposes = %v, want %v", typ, got, want[typ])
		}
		if got := a.CredentialReceiving; !maps.Equal(got, receiving[typ]) {
			t.Errorf("%s purposes tied to a way of receiving = %v, want %v", typ, got, receiving[typ])
		}
		if got := a.CredentialSending; !slices.Equal(got, sending[typ]) {
			t.Errorf("%s purposes tied to send = %v, want %v", typ, got, sending[typ])
		}
	}
}

// TestGraphNotificationsNeededWithNotificationURL: a microsoft.mail
// channel needs its notification secret exactly when its configuration
// asks for change notifications; without a notification URL it polls by
// choice.
func TestGraphNotificationsNeededWithNotificationURL(t *testing.T) {
	a, _ := registry(t, settings()).Lookup(microsoft.Type)
	ch := func(c microsoft.Config) model.Channel {
		return model.Channel{Type: microsoft.Type, CredentialRef: "m", Configuration: ext(t, microsoft.ConfigSchema, c)}
	}
	if got := a.ConditionalPurposes(ch(microsoft.Config{Mailbox: "a@example.com"})); len(got) != 0 {
		t.Errorf("polling channel needs %v", got)
	}
	got := a.ConditionalPurposes(ch(microsoft.Config{Mailbox: "a@example.com", NotificationURL: "https://heo.example.com/webhooks/x"}))
	if !slices.Equal(got, []string{microsoft.PurposeNotifications}) {
		t.Errorf("notifying channel needs %v", got)
	}
	if k := a.CredentialKinds[microsoft.PurposeNotifications]; len(k) != 1 || k[0] != ports.KindSecret {
		t.Errorf("notification secret kinds = %v", k)
	}
}
