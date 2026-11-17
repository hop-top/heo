package composition

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"hop.top/uma/adapter/chat/discord"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/adapter/chat/teams"
	"hop.top/uma/adapter/chat/telegram"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/adapter/mail/microsoft"
	metawhatsapp "hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/adapter/messaging/twilio/sms"
	twiliowhatsapp "hop.top/uma/adapter/messaging/twilio/whatsapp"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
)

// The provider account of each adapter's channel: what two channels
// share when they would receive the same traffic. uma's adapters expose
// no account key of their own, so heo derives it from each adapter's
// exported configuration type. heo qualifies every account by adapter
// type (adapters.Adapter.AccountOf), so only one adapter's channels are
// ever compared. Addresses and hosts compare case-insensitively.

// errConfiguration is a channel configuration that does not decode;
// the adapter's own validation names the field.
var errConfiguration = errors.New("channel configuration does not parse")

func decode(ch model.Channel, schema string, into any) error {
	if ch.Configuration.Name != schema {
		return errConfiguration
	}
	if err := json.Unmarshal(ch.Configuration.Data, into); err != nil {
		return errConfiguration
	}
	return nil
}

// channelAccount is a widget channel's account: the channel itself.
// Visitors reach a widget channel by its own route, so no two channels
// ever receive the same traffic.
func channelAccount(ch model.Channel) (string, error) {
	if ch.Ref.ID == "" {
		return "", errors.New("channel has no ID")
	}
	return "channel:" + string(ch.Ref.ID), nil
}

// smtpAccount is an smtp.mail channel's account: the listener its MX
// receiver binds, adapters.smtp.addr. Every receiving smtp.mail channel
// of a profile would bind it, so a profile has one; its recipient list
// maps each accepted address to an inbox destination.
func (o Options) smtpAccount(model.Channel) (string, error) {
	cfg, probs := ReadSMTP(o.viper())
	if err := problemsError(probs); err != nil {
		return "", err
	}
	if cfg.Addr == "" {
		return "", errNoSMTPListener
	}
	return "listen:" + strings.ToLower(cfg.Addr), nil
}

// imapAccount is an imap.mail channel's account: the mailbox folder it
// retrieves, on its IMAP server.
func imapAccount(ch model.Channel) (string, error) {
	cfg, issues := imap.ParseConfig(ch.Configuration)
	if len(issues) > 0 {
		return "", errConfiguration
	}
	if cfg.IMAP.Host == "" || cfg.Address == "" {
		return "", errors.New("configuration names no IMAP host or address")
	}
	folder := cfg.Mailbox
	if folder == "" {
		folder = imap.DefaultMailbox
	}
	return strings.ToLower(cfg.IMAP.Host) + "/" + strings.ToLower(cfg.Address) + "/" + folder, nil
}

// googleAccount is a google.mail channel's account: its Gmail user.
// A channel addressing "me" (no user) names the mailbox only through
// its credential, so the credential reference stands for it: two such
// channels on one reference read one mailbox.
func googleAccount(ch model.Channel) (string, error) {
	var cfg google.Config
	if err := decode(ch, google.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	if cfg.User != "" && cfg.User != "me" {
		return "user:" + strings.ToLower(cfg.User), nil
	}
	return credentialAccount(ch)
}

// microsoftAccount is a microsoft.mail channel's account: its mailbox,
// in its national cloud. A channel addressing the signed-in user (no
// mailbox) names it only through its credential, as googleAccount.
func microsoftAccount(ch model.Channel) (string, error) {
	var cfg microsoft.Config
	if err := decode(ch, microsoft.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	cloud := cfg.Cloud
	if cloud == "" {
		cloud = microsoft.CloudGlobal
	}
	if cfg.Mailbox != "" {
		return cloud + "/mailbox:" + strings.ToLower(cfg.Mailbox), nil
	}
	acct, err := credentialAccount(ch)
	if err != nil {
		return "", err
	}
	return cloud + "/" + acct, nil
}

// credentialAccount stands a channel's credential reference for the
// account the credential signs in as.
func credentialAccount(ch model.Channel) (string, error) {
	if ch.CredentialRef == "" {
		return "", errors.New("configuration names no mailbox and the channel has no credential reference")
	}
	return "credential:" + ch.CredentialRef, nil
}

// twilioSMSAccount is a twilio.sms channel's account: the sender it
// receives for, in its Twilio account.
func twilioSMSAccount(ch model.Channel) (string, error) {
	var cfg sms.Config
	if err := decode(ch, sms.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	return twilioAccount(cfg.AccountSID, cfg.From, cfg.MessagingServiceSID)
}

// twilioWhatsAppAccount is a twilio.whatsapp channel's account, as
// twilioSMSAccount.
func twilioWhatsAppAccount(ch model.Channel) (string, error) {
	var cfg twiliowhatsapp.Config
	if err := decode(ch, twiliowhatsapp.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	return twilioAccount(cfg.AccountSID, cfg.From, cfg.MessagingServiceSID)
}

// twilioAccount names a Twilio sender: a number, or a Messaging Service
// when the channel sends through one, which is where Twilio posts its
// incoming messages.
func twilioAccount(accountSID, from, service string) (string, error) {
	if accountSID == "" {
		return "", errors.New("configuration names no account SID")
	}
	switch {
	case service != "":
		return accountSID + "/service:" + service, nil
	case from != "":
		return accountSID + "/from:" + from, nil
	}
	return "", errors.New("configuration names no sender")
}

// metaWhatsAppAccount is a meta.whatsapp channel's account: its
// business phone number.
func metaWhatsAppAccount(ch model.Channel) (string, error) {
	var cfg metawhatsapp.Config
	if err := decode(ch, metawhatsapp.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	if cfg.PhoneNumberID == "" {
		return "", errors.New("configuration names no phone number ID")
	}
	return "phone:" + cfg.PhoneNumberID, nil
}

// discordAccount is a discord.chat channel's account: its application, on
// its REST host. An application has one bot token, so one gateway session
// (Discord closes the older of two), and one Interactions Endpoint URL, so
// two channels of one application would be handed the same traffic. A
// channel is the whole application, in every guild its bot is in: the
// configuration names no guild, and the account does not depend on how the
// channel receives.
func discordAccount(ch model.Channel) (string, error) {
	var cfg discord.Config
	if err := decode(ch, discord.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	if cfg.ApplicationID == "" {
		return "", errors.New("configuration names no application ID")
	}
	api := cfg.APIURL
	if api == "" {
		api = discord.DefaultAPIURL
	}
	u, err := url.Parse(api)
	if err != nil || u.Host == "" {
		return "", errConfiguration
	}
	return strings.ToLower(u.Host) + "/application:" + cfg.ApplicationID, nil
}

// slackAccount is a slack.chat channel's account: the bot user of the
// app installed in one workspace, on its Web API host. Slack posts every
// event and interaction of an install to the app's one request URL, so
// two channels on one workspace and bot would both be handed the same
// traffic.
func slackAccount(ch model.Channel) (string, error) {
	var cfg slack.Config
	if err := decode(ch, slack.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	if cfg.TeamID == "" || cfg.BotUserID == "" {
		return "", errors.New("configuration names no workspace or bot user")
	}
	api := cfg.APIURL
	if api == "" {
		api = slack.DefaultAPIURL
	}
	u, err := url.Parse(api)
	if err != nil || u.Host == "" {
		return "", errConfiguration
	}
	return strings.ToLower(u.Host) + "/team:" + cfg.TeamID + "/bot:" + cfg.BotUserID, nil
}

// telegramAccount is a telegram.messages channel's account: the bot, by
// its user ID, on its Bot API host. A bot takes its updates one way
// only, from its webhook or from getUpdates, whichever heo's channels
// use, so every channel of one bot would be handed the same updates
// (or, by getUpdates, would take them from one another): the claim is
// per bot, whether the channel receives by webhook or by polling.
func telegramAccount(ch model.Channel) (string, error) {
	var cfg telegram.Config
	if err := decode(ch, telegram.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	if cfg.BotID <= 0 {
		return "", errors.New("configuration names no bot ID")
	}
	api := cfg.APIURL
	if api == "" {
		api = telegram.DefaultAPIURL
	}
	u, err := url.Parse(api)
	if err != nil || u.Host == "" {
		return "", errConfiguration
	}
	return strings.ToLower(u.Host) + "/bot:" + strconv.FormatInt(cfg.BotID, 10), nil
}

// teamsAccount is a microsoft.teams channel's account: the bot
// registration, by its App ID, in its Microsoft Entra tenant. The Bot
// Connector posts every activity of a registration to its one messaging
// endpoint, so two channels of one bot would be handed the same
// activities. The service URL, where the bot sends, does not name what it
// receives; GUIDs compare case-insensitively.
func teamsAccount(ch model.Channel) (string, error) {
	var cfg teams.Config
	if err := decode(ch, teams.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	if cfg.AppID == "" || cfg.TenantID == "" {
		return "", errors.New("configuration names no app ID or tenant ID")
	}
	return "tenant:" + strings.ToLower(cfg.TenantID) + "/app:" + strings.ToLower(cfg.AppID), nil
}

// githubAccount is a github.issues channel's account: the GitHub user
// its credential acts as (account_id: the token owner, or the GitHub
// App's bot user), on its API host. A channel naming no account ID
// (webhooks only) names it only through its credential, as
// googleAccount.
func githubAccount(ch model.Channel) (string, error) {
	var cfg github.Config
	if err := decode(ch, github.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	api := cfg.APIURL
	if api == "" {
		api = github.DefaultAPIURL
	}
	u, err := url.Parse(api)
	if err != nil || u.Host == "" {
		return "", errConfiguration
	}
	host := strings.ToLower(u.Host)
	if cfg.AccountID > 0 {
		return host + "/account:" + strconv.FormatInt(cfg.AccountID, 10), nil
	}
	acct, err := credentialAccount(ch)
	if err != nil {
		return "", err
	}
	return host + "/" + acct, nil
}

// githubCapabilityOff names the capabilities a github.issues channel
// declares and reports unavailable until its replies are on (reason
// disabled at the provider): send, edit, delete and react, switched off
// by the channel configuration's replies key. A configuration that does
// not parse is left to the channel check.
func githubCapabilityOff(ch model.Channel) map[model.Capability]string {
	var cfg github.Config
	if err := decode(ch, github.ConfigSchema, &cfg); err != nil || cfg.Replies {
		return nil
	}
	const why = "replies is off in the channel configuration"
	return map[model.Capability]string{
		model.CapabilitySend: why, model.CapabilityEdit: why, model.CapabilityDelete: why, model.CapabilityReact: why,
	}
}

// zendeskAccount is a zendesk.tickets channel's account: its Zendesk
// account, by subdomain. Its receiver polls the account's ticket events,
// so two channels on one subdomain would read the same tickets.
func zendeskAccount(ch model.Channel) (string, error) {
	var cfg zendesk.Config
	if err := decode(ch, zendesk.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	if cfg.Subdomain == "" {
		return "", errors.New("configuration names no subdomain")
	}
	return "subdomain:" + strings.ToLower(cfg.Subdomain), nil
}
