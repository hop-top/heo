// Package composition is heo's composition root for uma adapters: the
// direct-message and ticket adapters a release binary runs, each
// registered explicitly with the host pieces it needs, and the HTTP
// client every adapter reaches its provider through.
//
// Nothing registers itself at init. Register adds, in order:
//
//   - native.webwidget (hop.top/uma/adapter/web/widget): browser chat,
//     receive_http plus an event stream mounted at <route>/events, with
//     each visitor's delivered events kept in the data store
//     (adapters.widget.retain per visitor) so a stream resumes across
//     restarts;
//   - smtp.mail (adapter/mail/smtp): an MX receiver on the listener
//     adapters.smtp.addr names, with STARTTLS when adapters.smtp.tls
//     names a certificate (smtptls.go), and relay submission;
//   - imap.mail (adapter/mail/imap): IMAP retrieval and SMTP submission;
//   - google.mail (adapter/mail/google): Gmail API, with optional Pub/Sub
//     push on the webhook; its label effect
//     (`conversation tag --add|--remove`) registered beside it;
//   - microsoft.mail (adapter/mail/microsoft): Microsoft Graph, with
//     optional change notifications on the webhook; its
//     category effect (`conversation tag --set`) registered beside it;
//   - twilio.sms and twilio.whatsapp (adapter/messaging/twilio);
//   - meta.whatsapp (adapter/messaging/meta/whatsapp): WhatsApp Cloud API;
//   - relay.messages (adapter/messaging/relay): SMS, WhatsApp and iMessage
//     through a gateway the operator runs, which posts each incoming message
//     to the channel's webhook route and takes each reply at a callback URL;
//     the hosts a callback may name are a host setting
//     (adapters.relay.callback_hosts), never a channel's, with the webhook
//     secret and the callback secret as two credential purposes;
//   - github.issues (adapter/ticket/github): GitHub issues and their
//     comments over the issues webhooks, one work item per issue;
//     its issue update effect (`issue update`) registered beside it;
//   - discord.chat (adapter/chat/discord): one Discord bot application,
//     its guilds and direct messages, received over the gateway (a
//     persistent websocket the receivers service holds), at the
//     Interactions Endpoint URL the channel's webhook route is, or both
//     (the channel's receive key picks), with the bot token and the
//     application's public key as two credential purposes;
//   - slack.chat (adapter/chat/slack): a Slack app installed in one
//     workspace, rooms, threads and direct messages, over the Events API
//     and interactions on the one request URL the channel's webhook route
//     is, or over Socket Mode, with the bot token, the signing secret
//     (request URL) and the app-level token (Socket Mode) as three
//     credential purposes; its modal effects (`interaction open-modal`,
//     `push-modal`, `update-modal`) registered beside it;
//   - telegram.messages (adapter/chat/telegram): one Telegram bot, chats,
//     forum topics and private chats, received by the bot's webhook or
//     by long polling (never both at Telegram), with the bot token and
//     the webhook secret token as two credential purposes;
//   - microsoft.teams (adapter/chat/teams): one Microsoft Teams bot (an App
//     ID in one Entra tenant), channels, threads, group and personal chats,
//     received on the Bot Framework messaging endpoint the channel's
//     webhook route is, each request verified by the adapter against the
//     Bot Framework's published keys, with the Bot Connector access token
//     as its one credential purpose;
//   - zendesk.tickets (adapter/ticket/zendesk): Zendesk Support tickets,
//     polled and over webhooks, one work item per ticket, with its
//     extension effects (the ticket update `ticket transition` submits,
//     and the internal note) registered beside it;
//   - gitlab.issues (adapter/ticket/gitlab): GitLab issues and their notes
//     over the issues and note webhooks, one work item per issue, on
//     gitlab.com or a self-managed instance (api_url);
//   - jira.issues (adapter/ticket/jira): Jira Cloud issues and their
//     comments over an admin webhook, one work item per issue; no state
//     change (it declares no work item act);
//   - linear.issues (adapter/ticket/linear): Linear issues and their
//     comments over the workspace webhook, one work item per issue; no
//     state change either. None of the three ships an extension effect.
//
// Each adapter names the provider account a channel connects to (see
// accounts.go), which heo's one-receiver-per-account checks key on.
package composition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/botauth"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/chat/discord"
	"hop.top/uma/adapter/chat/slack"
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
	"hop.top/uma/adapter/web/widget/storelog"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// The adapters flagged ReportsLink below, whose receivers tell the host
// whether their link is up: a build breaks if one stops.
var (
	_ adapter.ReceiverReporter = (*discord.Instance)(nil)
	_ adapter.ReceiverReporter = (*slack.Instance)(nil)
	_ adapter.ReceiverReporter = (*telegram.Instance)(nil)
	_ adapter.ReceiverReporter = (*imap.Instance)(nil)
)

// WidgetEventsRoute is where a widget channel's event stream is
// mounted, relative to the channel's webhook route.
const WidgetEventsRoute = "/events"

// WidgetSiteParam is the query parameter of a widget session request
// naming the site (inbox destination) the visitor is on. It may be
// omitted when the channel lists one site.
const WidgetSiteParam = "site"

// Options configures the adapters Register adds.
type Options struct {
	// Viper returns the resolved configuration, read for adapters.*
	// when a hook runs: when the smtp receiver binds, and when a
	// channel's provider account is computed. Nil reads the defaults.
	Viper func() *viper.Viper
	// Credentials opens the profile's credential backend, where the
	// smtp.mail listener's private key (adapters.smtp.tls.key_ref)
	// resolves. Nil: a configured listener certificate cannot load and
	// the listener does not bind.
	Credentials CredentialsFunc
	// Now is the clock the listener certificate's refresh runs on;
	// time.Now when nil.
	Now func() time.Time
	// Logger receives the microsoft.teams Connector token mint failures
	// and recoveries; nil drops them.
	Logger botauth.Logger
	// Store is the profile's data store, opened by the daemon as its
	// owner. The widget keeps each visitor's delivered events in it, so
	// they survive a restart. Nil keeps them in memory: only for
	// describing and validating channels, never for the runtime.
	Store store.Store
	// Listen binds the smtp.mail listener; net.ListenConfig's when nil.
	Listen ListenFunc

	// serving carries each smtp.mail listener bind to the receivers
	// service; Adapters sets it.
	serving *adapters.Serving
}

// ListenFunc binds a stream listener on network and address.
type ListenFunc func(ctx context.Context, network, address string) (net.Listener, error)

func (o Options) viper() *viper.Viper {
	if o.Viper != nil {
		if v := o.Viper(); v != nil {
			return v
		}
	}
	v := viper.New()
	for k, val := range Defaults() {
		v.SetDefault(k, val)
	}
	return v
}

// Register adds every adapter to r, explicitly.
func Register(r *adapters.Registry, o Options) error {
	list, err := Adapters(o)
	if err != nil {
		return err
	}
	for _, a := range list {
		if err := r.Register(a); err != nil {
			return err
		}
	}
	return nil
}

// Adapters returns the direct-message and ticket adapters, in
// registration order.
func Adapters(o Options) ([]adapters.Adapter, error) {
	o.serving = adapters.NewServing()
	log, err := o.widgetLog()
	if err != nil {
		return nil, err
	}
	w, err := widget.New(widget.Options{Verifier: widget.VerifierFunc(anonymousVisitor), Log: log})
	if err != nil {
		return nil, fmt.Errorf("widget adapter: %w", err)
	}
	rl, err := o.relayFactory()
	if err != nil {
		return nil, err
	}
	zd := zendesk.NewFactory()
	im := imap.NewFactory(imap.Options{})
	gm := google.NewFactory(googleOptions())
	mm := microsoft.NewFactory(microsoftOptions())
	gh := github.NewFactory(github.Options{})
	sl := slack.NewFactory(slack.Options{})
	tf := newTeamsFactory(o)
	return []adapters.Adapter{
		{Factory: w, Account: channelAccount, Routes: func(ch model.Channel) map[string]http.Handler {
			return map[string]http.Handler{WidgetEventsRoute: w.Events(ch.Ref)}
		}, CredentialPurposes: []string{widget.CredentialPurpose}},
		{Factory: smtpFactory{o: o, Factory: smtp.NewFactory(smtp.Options{Listen: o.listenSMTP})}, Account: o.smtpAccount, Serving: o.serving,
			CredentialPurposes: []string{smtp.PurposeRelay}},
		{Factory: im, Account: imapAccount, ReportsLink: true, Extensions: im.Extensions(),
			CredentialPurposes: []string{imap.PurposeIMAP, imap.PurposeSubmission}},
		{Factory: gm, Account: googleAccount, Staggered: true, Extensions: gm.Extensions(),
			CredentialPurposes: []string{google.PurposeGmail}},
		{Factory: mm, Account: microsoftAccount, Staggered: true, Extensions: mm.Extensions(),
			CredentialPurposes: []string{microsoft.PurposeGraph, microsoft.PurposeNotifications},
			CredentialKinds:    map[string][]ports.CredentialKind{microsoft.PurposeNotifications: {ports.KindSecret}},
			CredentialIf: map[string]adapters.CredentialCondition{microsoft.PurposeNotifications: {
				Needed: graphPushConfigured,
				Effect: "no change-notification subscription is created: mail arrives by delta polling only",
			}}},
		{Factory: sms.NewFactory(sms.Options{}), Account: twilioSMSAccount,
			CredentialPurposes: []string{sms.PurposeREST, sms.PurposeWebhook}},
		{Factory: twiliowhatsapp.NewFactory(twiliowhatsapp.Options{}), Account: twilioWhatsAppAccount,
			CredentialPurposes: []string{twiliowhatsapp.PurposeREST, twiliowhatsapp.PurposeWebhook}},
		{Factory: metawhatsapp.NewFactory(metawhatsapp.Options{}), Account: metaWhatsAppAccount,
			CredentialPurposes: []string{metawhatsapp.PurposeGraph, metawhatsapp.PurposeAppSecret, metawhatsapp.PurposeVerifyToken}},
		{Factory: rl, Account: relayAccount,
			CredentialPurposes: []string{relay.PurposeWebhook, relay.PurposeCallback},
			CredentialSending:  []string{relay.PurposeCallback}},
		{Factory: discord.NewFactory(), Account: discordAccount, ReportsLink: true,
			CredentialPurposes:  []string{discord.CredentialPurposeAPI, discord.CredentialPurposeInteractions},
			CredentialReceiving: map[string]model.Capability{discord.CredentialPurposeInteractions: model.CapabilityReceiveHTTP}},
		{Factory: sl, Account: slackAccount, ReportsLink: true, Extensions: sl.Extensions(),
			CredentialPurposes: []string{slack.CredentialPurposeAPI, slack.CredentialPurposeSigning, slack.CredentialPurposeSocket},
			CredentialReceiving: map[string]model.Capability{
				slack.CredentialPurposeSigning: model.CapabilityReceiveHTTP,
				slack.CredentialPurposeSocket:  model.CapabilityReceive,
			}},
		{Factory: telegram.NewFactory(telegram.Options{}), Account: telegramAccount, ReportsLink: true,
			CredentialPurposes:  []string{telegram.CredentialPurposeAPI, telegram.CredentialPurposeWebhook},
			CredentialReceiving: map[string]model.Capability{telegram.CredentialPurposeWebhook: model.CapabilityReceiveHTTP}},
		{Factory: tf, Account: teamsAccount,
			CredentialPurposes: []string{PurposeTeamsClientSecret, PurposeTeamsClientCertificate},
			CredentialOneOf:    [][]string{{PurposeTeamsClientSecret, PurposeTeamsClientCertificate}},
			CredentialKinds: map[string][]ports.CredentialKind{
				PurposeTeamsClientSecret:      {ports.KindSecret},
				PurposeTeamsClientCertificate: {ports.KindClientCert},
			},
			ProbeCredential: tf.Probe},
		{Factory: gh, Account: githubAccount, CapabilityOff: githubCapabilityOff, Extensions: gh.Extensions(),
			CredentialPurposes: []string{github.CredentialPurposeAPI, github.CredentialPurposeWebhook}},
		{Factory: zd, Account: zendeskAccount, Extensions: zd.Extensions(),
			CredentialPurposes: []string{zendesk.CredentialPurposeAPI, zendesk.CredentialPurposeWebhook}},
		{Factory: gitlab.NewFactory(gitlab.Options{}), Account: gitlabAccount, CapabilityOff: gitlabCapabilityOff,
			CredentialPurposes: []string{gitlab.CredentialPurposeAPI, gitlab.CredentialPurposeWebhook}},
		{Factory: jira.NewFactory(), Account: jiraAccount, CapabilityOff: jiraCapabilityOff,
			CredentialPurposes: []string{jira.CredentialPurposeAPI, jira.CredentialPurposeWebhook}},
		{Factory: linear.NewFactory(linear.Options{}), Account: linearAccount, CapabilityOff: linearCapabilityOff,
			CredentialPurposes: []string{linear.CredentialPurposeAPI, linear.CredentialPurposeWebhook}},
	}, nil
}

// widgetLog is the widget's visitor log: kept in o.Store, retaining
// adapters.widget.retain entries per visitor; nil, the widget's own
// in-memory default, without a store.
func (o Options) widgetLog() (widget.Log, error) {
	if o.Store == nil {
		return nil, nil
	}
	cfg, probs := ReadWidget(o.viper())
	if err := problemsError(probs); err != nil {
		return nil, fmt.Errorf("widget adapter: %w", err)
	}
	l, err := storelog.New(o.Store, storelog.Options{Retain: cfg.Retain})
	if err != nil {
		return nil, fmt.Errorf("widget adapter: %w", err)
	}
	return l, nil
}

// anonymousVisitor admits every widget visitor as anonymous: heo has
// no login of the embedding site to verify a user against. The site
// comes from the session request; the adapter refuses one the channel
// does not list.
func anonymousVisitor(r *http.Request, _ model.ChannelRef) (widget.Visitor, error) {
	return widget.Visitor{Assurance: widget.AssuranceAnonymous, Site: r.URL.Query().Get(WidgetSiteParam)}, nil
}

// errNoSMTPListener refuses an smtp.mail receiver while adapters.smtp.addr
// is empty.
var errNoSMTPListener = errors.New(KeySMTPAddr + " is not set: smtp.mail channels send only")

// listenSMTP binds the smtp.mail receiver's listener. The receiver
// closes it when its run ends.
func (o Options) listenSMTP(ctx context.Context, ch model.Channel) (net.Listener, error) {
	return o.bindSMTP(ctx, ch, nil)
}

// bindSMTP binds ch's listener once the settings check out and ready, if
// set, succeeds, and reports the bind, or its failure, as ch's receiver
// serving: a bound listener accepts connections at once.
func (o Options) bindSMTP(ctx context.Context, ch model.Channel, ready func(context.Context) error) (net.Listener, error) {
	cfg, probs := ReadSMTP(o.viper())
	if err := problemsError(probs); err != nil {
		return nil, model.NewError(model.Permanent, err.Error())
	}
	if cfg.Addr == "" {
		return nil, model.NewError(model.Permanent, errNoSMTPListener.Error())
	}
	if ready != nil {
		if err := ready(ctx); err != nil {
			return nil, err
		}
	}
	listen := o.Listen
	if listen == nil {
		var lc net.ListenConfig
		listen = lc.Listen
	}
	ln, err := listen(ctx, "tcp", cfg.Addr)
	o.serving.Report(ch.Ref, err)
	return ln, err
}

// smtpFactory opens smtp.mail instances with the STARTTLS configuration
// the settings name when the channel opens: a listener certificate
// offers STARTTLS, none offers none. Describe and Validate are uma's.
type smtpFactory struct {
	*smtp.Factory
	o Options
}

// Open opens the instance. With a listener certificate configured, its
// listener binds only once the pair loads, so the receiver never serves
// without the TLS it was configured with.
func (f smtpFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	cfg, _ := ReadSMTP(f.o.viper())
	if !cfg.TLS() {
		return f.Factory.Open(ctx, ch, deps)
	}
	src := newTLSSource(cfg, f.o.Credentials, f.o.Now)
	listen := func(ctx context.Context, ch model.Channel) (net.Listener, error) {
		return f.o.bindSMTP(ctx, ch, func(ctx context.Context) error {
			if _, err := src.get(ctx); err != nil {
				return model.NewError(model.Permanent, "smtp.mail listener certificate: "+err.Error())
			}
			return nil
		})
	}
	return smtp.NewFactory(smtp.Options{Listen: listen, TLS: src.config()}).Open(ctx, ch, deps)
}

// graphPushConfigured reports whether a microsoft.mail channel asks for
// change notifications (a notification_url): without one it polls, by
// choice, and needs no notification secret.
func graphPushConfigured(ch model.Channel) bool {
	var c microsoft.Config
	if err := json.Unmarshal(ch.Configuration.Data, &c); err != nil {
		return false
	}
	return c.NotificationURL != ""
}
