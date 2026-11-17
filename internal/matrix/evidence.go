package matrix

import "hop.top/uma/model"

// Kind says where a row's behavior was exercised. The values are the
// words the published matrix uses.
type Kind string

const (
	// RealSandbox: a provider account the maintainers own, through
	// recorded cassettes that replay in `go test`.
	RealSandbox Kind = "real private sandbox"
	// RealLiveAccount: a live provider account the maintainers own that
	// bills or reaches a person, so the recording was made with a hard cap
	// on what it sent, to the owner's own number only, through recorded
	// cassettes that replay in `go test`.
	RealLiveAccount Kind = "real live account, 2 sends"
	// Fake: heo's own fake provider adapter, driven through the binary.
	Fake Kind = "fake provider"
	// InProcess: the real adapter inside the process, with no provider
	// involved (uma is the provider, or the request is queued and never
	// sent).
	InProcess Kind = "in-process"
	// None: nothing exercises the behavior.
	None Kind = "none"
)

// TestRef is a Go test a row cites.
type TestRef struct {
	// Name is the test function's name; the matrix test finds it in the
	// repository's test files.
	Name string
	// Behavior is true when the test runs the verb or the adapter's
	// work, false when it checks only what is declared or that a thing
	// starts.
	Behavior bool
}

func behavior(names ...string) []TestRef { return refs(true, names) }
func declared(names ...string) []TestRef { return refs(false, names) }

func refs(b bool, names []string) []TestRef {
	out := make([]TestRef, 0, len(names))
	for _, n := range names {
		out = append(out, TestRef{Name: n, Behavior: b})
	}
	return out
}

// DeclarationTest checks, for every maintained adapter, which verbs its
// descriptor enables. It backs every row's `declared` column.
const DeclarationTest = "TestDeclaredVerbsPerAdapter"

// Evidence is what backs one declared adapter and verb: the tests that
// run it and where they ran it. A row claims no more than this says.
type Evidence struct {
	Tests   []TestRef
	Against Kind
}

// historyPreview is the test that previews `history fetch` on channels
// of the maintained adapters declaring history: it reads their real
// declaration and imports nothing.
const historyPreview = "TestHistoryFetchPreviewsOnMaintainedAdapters"

// githubCassettes is the one recording of github.issues.
const githubCassettes = "TestXRRGitHubIssuesProviderVerbs"

// discordCassettes is the one recording of discord.chat.
const discordCassettes = "TestXRRDiscordChatProviderVerbs"

// discordVerbs is discord.chat's provider verbs, through the daemon
// against a scripted Discord (REST and gateway).
const discordVerbs = "TestDiscordProviderVerbs"

// discordThreads is discord.chat's thread membership and per-thread
// answers.
const discordThreads = "TestDiscordSelectionRoutes"

// slackVerbs is slack.chat's provider verbs, through the daemon against a
// scripted Slack.
const slackVerbs = "TestSlackProviderVerbs"

// slackModals is slack.chat's modal verbs (views.open, views.push and
// views.update), through the daemon against a scripted Slack.
const slackModals = "TestSlackModalVerbs"

// telegramVerbs is telegram.messages's provider verbs, through the daemon
// against a scripted Bot API.
const telegramVerbs = "TestTelegramProviderVerbs"

// teamsVerbs is microsoft.teams's provider verbs, through the daemon
// against a scripted Bot Connector.
const teamsVerbs = "TestTeamsProviderVerbs"

// twilioCassettes is the one recording of twilio.sms.
const twilioCassettes = "TestXRRTwilioSMSProviderVerbs"

// gmailHistory and graphHistory replay the answers uma recorded from a
// private mailbox; imapHistory runs against an in-process IMAP server.
const (
	gmailHistory = "TestMailHistoryGmailCassettes"
	graphHistory = "TestMailHistoryGraphCassettes"
	imapHistory  = "TestMailHistoryIMAP"
)

// evidence is the explicit map from a declared adapter and verb to its
// tests. Every declared pair must have an entry, even one that says
// None: the matrix test fails otherwise, and fails an entry whose
// pair no adapter declares.
var evidence = map[string]Evidence{
	// native.webwidget: uma is the widget's provider, so there is no
	// provider to record; the whole path runs in process through heo.
	key(model.TypeNativeWebWidget, "message edit"):   {behavior("TestWidgetProviderVerbs"), InProcess},
	key(model.TypeNativeWebWidget, "message delete"): {behavior("TestWidgetProviderVerbs"), InProcess},
	key(model.TypeNativeWebWidget, "reaction add"):   {behavior("TestWidgetProviderVerbs"), InProcess},
	// The widget test adds a reaction and never removes one.
	key(model.TypeNativeWebWidget, "reaction remove"): {nil, None},
	// The widget declares receipt (it tells a visitor's browser a message
	// was read); nothing in heo sends one through it.
	key(model.TypeNativeWebWidget, "receipt send"): {nil, None},
	// The widget declares respond and respond_follow_up: a visitor's press
	// is answered in each mode, as a follow-up too, and the visitor's event
	// stream carries each answer.
	key(model.TypeNativeWebWidget, "interaction respond"): {behavior("TestWidgetInteractionVerbs"), InProcess},

	// github.issues: every verb recorded against a private sandbox
	// repository. The status change is also run in process with the
	// workers off (queued, never sent).
	key(model.TypeGithubIssues, "message edit"):        {behavior(githubCassettes), RealSandbox},
	key(model.TypeGithubIssues, "message delete"):      {behavior(githubCassettes), RealSandbox},
	key(model.TypeGithubIssues, "reaction add"):        {behavior(githubCassettes), RealSandbox},
	key(model.TypeGithubIssues, "reaction remove"):     {behavior(githubCassettes), RealSandbox},
	key(model.TypeGithubIssues, "conversation status"): {behavior(githubCassettes, "TestConversationStatusGitHubAdapter"), RealSandbox},
	// The field update is previewed and queued in process with the real adapter's
	// declarations and validation; nothing is sent, so it is not recorded.
	key(model.TypeGithubIssues, "conversation update"): {behavior("TestUpdateRoutesByWhatTheAdapterDeclares", "TestConversationUpdateLifecycle"), InProcess},
	// github.issues declares history too; the import is not recorded. Only
	// the preview, which reads the declaration, runs against the real
	// adapter.
	key(model.TypeGithubIssues, "history fetch"): {declared(historyPreview), None},

	// twilio.sms declares history; the import is not recorded either (its
	// two live sends are spent on the send and reply).
	key(model.TypeTwilioSms, "history fetch"): {declared(historyPreview), None},

	// discord.chat: edit, delete and reactions recorded in a throwaway
	// server the maintainers own. Every declared verb also runs through the
	// real adapter, the daemon's workers and heo's HTTP seam against a
	// scripted Discord. The recording refuses thread membership and
	// interaction callbacks, so those pin the request the adapter makes, not
	// what Discord answers. Membership is a thread's members; history is
	// declared and only its preview runs.
	key(model.TypeDiscordChat, "message edit"):        {behavior(discordCassettes, discordVerbs), RealSandbox},
	key(model.TypeDiscordChat, "message delete"):      {behavior(discordCassettes, discordVerbs), RealSandbox},
	key(model.TypeDiscordChat, "reaction add"):        {behavior(discordCassettes, discordVerbs), RealSandbox},
	key(model.TypeDiscordChat, "reaction remove"):     {behavior(discordCassettes, discordVerbs), RealSandbox},
	key(model.TypeDiscordChat, "participant add"):     {behavior(discordThreads), InProcess},
	key(model.TypeDiscordChat, "participant remove"):  {behavior(discordThreads), InProcess},
	key(model.TypeDiscordChat, "interaction respond"): {behavior(discordVerbs), InProcess},
	key(model.TypeDiscordChat, "history fetch"):       {declared(historyPreview), None},

	// slack.chat: no workspace to record against yet. Every declared verb
	// runs through the real adapter, the daemon's workers and heo's HTTP
	// seam against a scripted Web API and response URL, so what is pinned
	// is the request the adapter makes, not what Slack answers.
	key(model.TypeSlackChat, "message edit"):        {behavior(slackVerbs), InProcess},
	key(model.TypeSlackChat, "message delete"):      {behavior(slackVerbs), InProcess},
	key(model.TypeSlackChat, "reaction add"):        {behavior(slackVerbs), InProcess},
	key(model.TypeSlackChat, "reaction remove"):     {behavior(slackVerbs), InProcess},
	key(model.TypeSlackChat, "interaction respond"): {behavior(slackVerbs), InProcess},
	// The modal verbs run slack.chat's own effects through the daemon's
	// workers against the scripted Web API; no workspace has run them.
	key(model.TypeSlackChat, "interaction open-modal"):   {behavior(slackModals), InProcess},
	key(model.TypeSlackChat, "interaction push-modal"):   {behavior(slackModals), InProcess},
	key(model.TypeSlackChat, "interaction update-modal"): {behavior(slackModals), InProcess},
	// slack.chat declares history too; reading it is not recorded or run:
	// only the preview, which reads the declaration, runs against the real
	// adapter.
	key(model.TypeSlackChat, "history fetch"): {declared(historyPreview), None},

	// telegram.messages: no bot to record against yet. Every declared verb
	// runs through the real adapter, the daemon's workers and heo's HTTP
	// seam against a scripted Bot API, so what is pinned is the request
	// the adapter makes, not what Telegram answers. It declares no
	// history: the Bot API cannot read past messages.
	key(model.TypeTelegramMessages, "message edit"):        {behavior(telegramVerbs), InProcess},
	key(model.TypeTelegramMessages, "message delete"):      {behavior(telegramVerbs), InProcess},
	key(model.TypeTelegramMessages, "reaction add"):        {behavior(telegramVerbs), InProcess},
	key(model.TypeTelegramMessages, "reaction remove"):     {behavior(telegramVerbs), InProcess},
	key(model.TypeTelegramMessages, "interaction respond"): {behavior(telegramVerbs), InProcess},

	// microsoft.teams: no tenant to record against yet. Every declared verb
	// runs through the real adapter, the daemon's workers and heo's HTTP
	// seam against a scripted Bot Connector, so what is pinned is the
	// request the adapter makes, not what Teams answers. It declares no
	// history and no respond: the Bot Connector cannot read past messages,
	// and an Action.Execute is answered by a reply or an edit.
	key(model.TypeMicrosoftTeams, "message edit"):    {behavior(teamsVerbs), InProcess},
	key(model.TypeMicrosoftTeams, "message delete"):  {behavior(teamsVerbs), InProcess},
	key(model.TypeMicrosoftTeams, "reaction add"):    {behavior(teamsVerbs), InProcess},
	key(model.TypeMicrosoftTeams, "reaction remove"): {behavior(teamsVerbs), InProcess},

	// meta.whatsapp declares the reactions; nothing in heo runs them.
	key(model.TypeMetaWhatsapp, "reaction add"):    {nil, None},
	key(model.TypeMetaWhatsapp, "reaction remove"): {nil, None},

	// message raw reads the provider's copy of a message: the verb runs
	// against heo's fake provider that declares raw_read, through the
	// daemon; the mail adapters' own read (Gmail's format=raw, Graph's
	// $value, IMAP's BODY.PEEK[]) is uma's and tested there, and no
	// mailbox has served it to heo.
	key(model.TypeImapMail, "message raw"):      {nil, None},
	key(model.TypeGoogleMail, "message raw"):    {nil, None},
	key(model.TypeMicrosoftMail, "message raw"): {nil, None},

	// google.mail and microsoft.mail: no mailbox with OAuth credentials. The tag
	// change is previewed and queued in process with the real adapters' declarations
	// and validation; nothing is sent. Gmail's labels and Outlook's categories are
	// each the one effect the adapter ships.
	key(model.TypeGoogleMail, "conversation tag"):    {behavior("TestTagQueuesMailEffects", "TestTagPreviewChecksTheFields"), InProcess},
	key(model.TypeMicrosoftMail, "conversation tag"): {behavior("TestTagQueuesMailEffects", "TestTagPreviewChecksTheFields"), InProcess},
	// The reply draft and the unsubscribe run each adapter's own effect
	// the same way: previewed and queued in process against the real
	// adapters' declarations and validation, and held for a plugin actor
	// and applied once on approval against uma's store. No request reaches
	// Gmail, Graph or a sender; the adapters' own handling of a
	// List-Unsubscribe header (DKIM, one-click, the host guard) is uma's
	// and is tested there, not here.
	key(model.TypeGoogleMail, "message reply-draft"):         {behavior("TestReplyDraftQueuesMailEffects", "TestReplyDraftPreviewChecks", "TestHeldReplyDraft"), InProcess},
	key(model.TypeMicrosoftMail, "message reply-draft"):      {behavior("TestReplyDraftQueuesMailEffects", "TestReplyDraftPreviewChecks", "TestHeldReplyDraft"), InProcess},
	key(model.TypeGoogleMail, "conversation unsubscribe"):    {behavior("TestUnsubscribeQueuesMailEffects", "TestUnsubscribePreviewChecks", "TestHeldUnsubscribe"), InProcess},
	key(model.TypeMicrosoftMail, "conversation unsubscribe"): {behavior("TestUnsubscribeQueuesMailEffects", "TestUnsubscribePreviewChecks", "TestHeldUnsubscribe"), InProcess},

	// imap.mail, google.mail and microsoft.mail declare history import. Gmail
	// and Graph run it through the daemon against the answers of uma's own
	// recordings of a private mailbox (cassettes copied from uma, replayed by
	// a local server); IMAP has no recording and runs against an in-process
	// IMAP server.
	key(model.TypeImapMail, "history fetch"):      {behavior(imapHistory), InProcess},
	key(model.TypeGoogleMail, "history fetch"):    {behavior(gmailHistory), RealSandbox},
	key(model.TypeMicrosoftMail, "history fetch"): {behavior(graphHistory), RealSandbox},

	// zendesk.tickets: no sandbox account. The update is previewed and
	// queued in process with the real adapter's declarations; nothing is
	// sent. The offline validation of the action is a unit test against a
	// stub validator.
	// gitlab.issues, jira.issues and linear.issues: no GitLab, Jira or Linear
	// account to record against, and no heo test runs their edit or delete
	// through the daemon: the verbs are enabled by the adapters' declarations
	// alone (TestDeclaredVerbsPerAdapter), so nothing is claimed beyond it.
	// gitlab.issues declares history too; only the preview, which reads the
	// declaration, runs. It declares the close and the reopen, so
	// `conversation status` is enabled on it as on github.issues; it is
	// run in process against the adapter's declarations with no provider
	// (the workers are off), and not recorded.
	key(model.TypeGitlabIssues, "message edit"):        {nil, None},
	key(model.TypeGitlabIssues, "message delete"):      {nil, None},
	key(model.TypeGitlabIssues, "history fetch"):       {declared(historyPreview), None},
	key(model.TypeGitlabIssues, "conversation status"): {behavior("TestStatusClosesAndReopensGitLabIssues"), InProcess},
	key(model.TypeJiraIssues, "message edit"):          {nil, None},
	key(model.TypeJiraIssues, "message delete"):        {nil, None},
	key(model.TypeLinearIssues, "message edit"):        {nil, None},
	key(model.TypeLinearIssues, "message delete"):      {nil, None},

	key(model.TypeZendeskTickets, "conversation update"): {
		append(behavior("TestUpdateRoutesByWhatTheAdapterDeclares", "TestWorkItemsThroughConversations"), declared("TestConversationUpdateLifecycle")...),
		InProcess,
	},
}

// stateVerbs are the conversation state verbs, in the verb table's order,
// and the adapters' capabilities that carry them.
var (
	stateVerbs     = []string{"conversation archive", "conversation unarchive", "conversation move", "conversation read", "conversation unread", "conversation flag", "conversation unflag", "conversation trash", "conversation untrash"}
	archiveVerbs   = stateVerbs[:2]
	stateTestNames = []string{"TestStateQueuesProviderStateOperations", "TestStatePreviewReadsTheChannelsLimits", "TestHeldStateChanges"}
)

// The conversation state verbs. google.mail and microsoft.mail (every one
// of the nine) run in process against the real adapters' declarations,
// limits and gate, with the engine's own queue and hold: previewed,
// queued and held, no request reaches Gmail or Graph, and what either
// provider answers to a state change is uma's to test, not heo's. The
// IMAP adapter and the chat adapters (slack.chat and discord.chat declare
// archive only) are enabled by their declarations alone, which
// TestDeclaredVerbsPerAdapter pins; no heo test runs their state changes,
// so nothing is claimed beyond it.
func init() {
	for _, t := range []model.TypeKey{model.TypeGoogleMail, model.TypeMicrosoftMail} {
		for _, v := range stateVerbs {
			evidence[key(t, v)] = Evidence{behavior(stateTestNames...), InProcess}
		}
	}
	for _, v := range stateVerbs {
		evidence[key(model.TypeImapMail, v)] = Evidence{nil, None}
	}
	for _, t := range []model.TypeKey{model.TypeSlackChat, model.TypeDiscordChat} {
		for _, v := range archiveVerbs {
			evidence[key(t, v)] = Evidence{nil, None}
		}
	}
}

func key(t model.TypeKey, verb string) string { return string(t) + " / " + verb }

// gates are the tests of the refusal every provider-dependent verb makes
// when an adapter does not declare it: the preview and the submission
// both fail UNSUPPORTED and nothing is written. The adapter under test is
// heo's fake provider, which declares nothing, so the gate is shown
// once per verb, not once per adapter.
var gates = []struct {
	Verb  string
	Tests []string
}{
	{"message edit", []string{"TestMessageEditLifecycle"}},
	{"message delete", []string{"TestMessageDeleteLifecycle"}},
	{"reaction add", []string{"TestReactionLifecycle"}},
	{"reaction remove", []string{"TestReactionLifecycle"}},
	{"participant add", []string{"TestParticipantLifecycle"}},
	{"participant remove", []string{"TestParticipantLifecycle"}},
	{"receipt send", []string{"TestReceiptSendLifecycle"}},
	{"history fetch", []string{"TestHistoryFetchLifecycle"}},
	{"interaction respond", []string{"TestInteractionRespondLifecycle"}},
	{"interaction open-modal", []string{"TestModalRefusals"}},
	{"interaction push-modal", []string{"TestModalRefusals"}},
	{"interaction update-modal", []string{"TestModalRefusals"}},
	{"conversation status", []string{"TestConversationStatusLifecycle"}},
	{"conversation update", []string{"TestConversationUpdateLifecycle", "TestChannelVerbsOfflineAndLive"}},
	{"conversation tag", []string{"TestConversationTagLifecycle"}},
	{"message raw", []string{"TestMessageRawLifecycle"}},
	{"message reply-draft", []string{"TestMessageReplyDraftLifecycle"}},
	{"conversation unsubscribe", []string{"TestConversationUnsubscribeLifecycle"}},
	{"conversation archive", []string{"TestConversationStateLifecycle"}},
	{"conversation unarchive", []string{"TestConversationStateLifecycle"}},
	{"conversation move", []string{"TestConversationStateLifecycle"}},
	{"conversation read", []string{"TestConversationStateLifecycle"}},
	{"conversation unread", []string{"TestConversationStateLifecycle"}},
	{"conversation flag", []string{"TestConversationStateLifecycle"}},
	{"conversation unflag", []string{"TestConversationStateLifecycle"}},
	{"conversation trash", []string{"TestConversationStateLifecycle"}},
	{"conversation untrash", []string{"TestConversationStateLifecycle"}},
}

// StatusOf is the recording status of typ.
func StatusOf(typ model.TypeKey) (Status, bool) {
	s, ok := statuses[typ]
	return s, ok
}

// Status is where an adapter stands as a whole, beyond its verbs.
type Status struct {
	Against Kind
	// Summary says what was and was not recorded.
	Summary string
	// Needs says what recording the rest takes.
	Needs string
	Tests []TestRef
}

// statuses is the recording status of each maintained adapter. An
// adapter with no entry fails the matrix test.
var statuses = map[model.TypeKey]Status{
	model.TypeGithubIssues: {
		Against: RealSandbox,
		Summary: "reply, edit, delete, react, unreact, close, reopen and close as not planned recorded; inbound " +
			"replayed from webhook fixtures written from real answers; error answers (401, 403, 404, 422, 429) and reconcile not recorded; " +
			"its own extension_execute effect `github.issues.update` (assignees, labels, milestone) is run by `issue update`: " +
			"previewed and queued in process with the adapter's own validation, nothing sent, so not recorded; what GitHub answers to it is not exercised",
		Needs: "a private sandbox repository you own and a token for its issues: `make record-github`; recording `conversation update` also needs, in that repository, " +
			"a collaborator login, an existing milestone and a label to set",
		Tests: behavior(githubCassettes),
	},
	model.TypeGitlabIssues: {
		Against: None,
		Summary: "not recorded: no GitLab account to record against; only that the adapter registers, names its account and reports send, edit and delete " +
			"off until replies is on is tested; its webhook, notes, edits, deletions and history are uma's, not run through heo; its close and reopen run through `conversation status` in process against the adapter's declarations, no provider",
		Needs: "a throwaway GitLab project you own and a token for it (`api` scope), and the webhook's secret token",
		Tests: declared("TestRegisterAdapters", "TestProviderAccounts", "TestIssueRepliesOff"),
	},
	model.TypeJiraIssues: {
		Against: None,
		Summary: "not recorded: no Jira Cloud site to record against; only that the adapter registers, names its account and reports send, edit, delete and reconcile off until replies is on is tested; its admin webhook, " +
			"comments, edits and deletions are uma's, not run through heo; it declares no work item act, so no state is changed from heo",
		Needs: "a Jira Cloud site you own, an API token and an admin webhook with a secret",
		Tests: declared("TestRegisterAdapters", "TestProviderAccounts", "TestIssueRepliesOff"),
	},
	model.TypeLinearIssues: {
		Against: None,
		Summary: "not recorded: no Linear workspace to record against; only that the adapter registers, names its account and reports send, edit and delete off until replies is on is tested; its webhook, " +
			"comments, edits and deletions are uma's, not run through heo; it declares no work item act, so no state is changed from heo",
		Needs: "a throwaway Linear workspace you own, an API key or OAuth token and a webhook with a signing secret",
		Tests: declared("TestRegisterAdapters", "TestProviderAccounts", "TestIssueRepliesOff"),
	},
	model.TypeNativeWebWidget: {
		Against: InProcess,
		Summary: "no external provider: uma is the provider, so there is nothing to record; every verb runs in process",
		Needs:   "nothing",
		Tests:   behavior("TestWidgetProviderVerbs", "TestWidgetInteractionVerbs"),
	},
	model.TypeDiscordChat: {
		Against: RealSandbox,
		Summary: "a first message to a channel, a reply, an edit, a reaction added and removed and two deletions recorded in a throwaway server; " +
			"a thread's members and an interaction answered in each mode, which the recording refuses, run through the real adapter and the daemon's workers against a " +
			"scripted Discord, the gateway receiver (identify, resume after a close, a reconnect and a restart, the close that ends it for good) " +
			"runs over a WebSocket of the test's own through heo's HTTP client, and the Interactions Endpoint URL's Ed25519 checks run on the " +
			"real listener; Discord's error answers, rate limits, the gateway's own behavior and history are not exercised",
		Needs: "a Discord application you own with its bot in one throwaway server and its bot token: `make record-discord`, at most 3 sends ever; " +
			"thread membership, interaction answers and the gateway need a recording that allows them, and a person pressing a button",
		Tests: behavior(discordCassettes, discordVerbs, discordThreads, "TestDiscordGateway", "TestDiscordInteractionsEndpoint"),
	},
	model.TypeSlackChat: {
		Against: InProcess,
		Summary: "not recorded: no Slack workspace yet; a first message to a room, a reply, an edit, a reaction added and removed, a deletion " +
			"and an interaction response with its follow-up run through the real adapter and the daemon's workers against a scripted Web API " +
			"and response URL, and the webhook's signature checks run on the real listener; what Slack answers (error codes, rate limits, " +
			"metadata schema refusals), history and the Socket Mode receiver (`receive`, experimental) are not exercised; its own extension_execute effects " +
			"`slack.chat.views.open`, `slack.chat.views.push` and `slack.chat.views.update` (modals) are run by `interaction open-modal`, `push-modal` and " +
			"`update-modal`: a modal opened from a recorded press, a view updated by its ID, the trigger_id's three seconds spent inside heo, a late " +
			"attempt refused with no call and a lost answer left unknown and never sent again run through the real adapter and the workers against the " +
			"scripted Web API; whether Slack accepts a view, and every code it answers a trigger_id with, are not exercised",
		Needs: "a Slack workspace you own with the app installed, its bot token, signing secret and a throwaway channel: `make record-slack`, at most 3 posts ever; " +
			"recording the modal verbs also needs interactivity enabled on the app (a request URL) and a person pressing a button inside three seconds",
		Tests: behavior(slackVerbs, "TestSlackWebhook", slackModals),
	},
	model.TypeTelegramMessages: {
		Against: InProcess,
		Summary: "not recorded: no bot yet; a first message to a chat, a reply, an edit, a reaction set and cleared, a deletion and an " +
			"interaction response in each mode run through the real adapter and the daemon's workers against a scripted Bot API, the " +
			"webhook's secret token checks run on the real listener, and long polling runs against the same stand-in; what Telegram answers " +
			"(the phrases of its refusals, flood limits, a callback query too old) and the reaction list are not exercised",
		Needs: "a bot you own (BotFather), its token, the bot's username and one throwaway group the bot is in: `make record-telegram`, at most 4 sends ever",
		Tests: behavior(telegramVerbs, "TestTelegramWebhook", "TestTelegramPolling"),
	},
	model.TypeMicrosoftTeams: {
		Against: InProcess,
		Summary: "not recorded: no tenant yet; a first message to a conversation, a reply, an edit, a reaction set and cleared, a deletion " +
			"and the personal chat a user route opens run through the real adapter and the daemon's workers against a scripted Bot Connector, " +
			"and the Bot Connector token checks of the messaging endpoint (issuer, audience, expiry, signature, service URL, endorsement) run " +
			"on the real listener against scripted signing keys; what Teams answers (the phrases of its refusals, throttling, what the client " +
			"shows for `textFormat` plain, the invoke response, a reaction in a channel thread) and the activities Teams posts are not exercised",
		Needs: "a Microsoft Entra tenant you own with an Azure Bot registered single tenant, its app installed for your own user, and a Bot Connector " +
			"token you mint from the app's client secret by hand: `make record-teams`, at most 3 sends ever",
		Tests: behavior(teamsVerbs, "TestTeamsWebhook"),
	},
	model.TypeTwilioSms: {
		Against: RealLiveAccount,
		Summary: "a send that starts a conversation and a reply recorded against a live Twilio account: two texts to the maintainer's own phone, " +
			"and a text to a number too short to exist, which Twilio rejects (error 21265) with no message created; incoming text and " +
			"status callback replayed from unsigned fixtures the test signs, since no public webhook URL exists; delivery to the phone, " +
			"the settling of a send by its status callback, media, a messaging service and the other error answers (401, 429, 5xx) not recorded",
		Needs: "a Twilio account you own, its auth token, one SMS number and your own phone number as the only recipient: `make record-twilio`, at most 2 sends ever",
		Tests: behavior(twilioCassettes),
	},
	model.TypeTwilioWhatsapp: {
		Against: None,
		Summary: "not recorded: no WhatsApp sender on the account (the twilio.sms sends do not cover it); only that the channel starts and its webhook refuses with no credential stored is tested",
		Needs:   "a Twilio account with a WhatsApp sender or the sandbox, and the signed webhooks replayed locally",
		Tests:   declared("TestRuntimeDirectMessageChannels"),
	},
	model.TypeMetaWhatsapp: {
		Against: None,
		Summary: "not recorded: no sandbox account; only that the channel starts and its webhook refuses with no credential stored is tested",
		Needs:   "a Meta app in development mode with a WhatsApp Business test number and a Graph API token",
		Tests:   declared("TestRuntimeDirectMessageChannels"),
	},
	model.TypeRelayMessages: {
		Against: InProcess,
		Summary: "not recorded: the adapter is the operator's own relay, so it has no provider of its own to record; the gateway's webhook (signed and bearer, " +
			"each refusal), a first message and a reply through the callback, a lost answer settled by the lookup and one with no lookup left unknown " +
			"run through the real adapter, the daemon's workers and heo's HTTP seam against an in-process gateway; the reference gateway in front of " +
			"Blooio or BlueBubbles, what an iMessage backend sends and delivery to a device are not exercised. It declares no heo provider verb",
		Needs: "an iMessage backend you own (a Blooio line, or BlueBubbles Server on a Mac), uma's reference gateway in front of it and your own handle " +
			"as the only recipient: `make record-imessage`, at most 2 sends ever",
		Tests: behavior("TestRelayWebhook", "TestRelaySend", "TestRelayLostAnswerIsLookedUp", "TestRelayWithoutLookupStaysUnknown"),
	},
	model.TypeZendeskTickets: {
		Against: InProcess,
		Summary: "not recorded: no sandbox account; a ticket field update (its status included) is previewed and queued in process, nothing reaches Zendesk",
		Needs:   "a Zendesk Support sandbox subdomain and an API token",
		Tests:   behavior("TestUpdateRoutesByWhatTheAdapterDeclares", "TestWorkItemsThroughConversations"),
	},
	model.TypeSmtpMail: {
		Against: InProcess,
		Summary: "xrr cannot record SMTP; the MX listener is tested in process (it greets and offers STARTTLS); delivery through a relay is not recorded",
		Needs:   "a relay the test owns, since SMTP has no cassette",
		Tests:   append(declared("TestRuntimeDirectMessageChannels"), behavior("TestSMTPStartTLSOffered")...),
	},
	model.TypeImapMail: {
		Against: InProcess,
		Summary: "xrr cannot record IMAP; `history fetch` runs through the daemon against an in-process IMAP server (a window, a cursor, attachment bytes, a conversation's sender, the listed mailboxes); " +
			"sending, receiving and the server's own behavior are not exercised, and the receiver is only tested to report its channel when no credential is stored",
		Needs: "a mailbox the test owns, since IMAP has no cassette",
		Tests: append(declared("TestRuntimeDirectMessageChannels"), behavior(imapHistory)...),
	},
	model.TypeGoogleMail: {
		Against: RealSandbox,
		Summary: "only `history fetch` is replayed, from the answers uma recorded from a private mailbox (a window of three messages, one with an attachment, read in pages with a cursor), " +
			"through the daemon; not recorded by heo, and sending, receiving, a conversation's sender and the other error answers are not exercised; its push route refusing an unsigned request is tested; " +
			"its own extension_execute effects are run by `conversation tag --add|--remove` (`google.mail.labels`), `message reply-draft` (`google.mail.reply_draft`) " +
			"and `conversation unsubscribe` (`google.mail.unsubscribe`): previewed and queued in process with the adapter's own validation, nothing sent, so not recorded; " +
			"no unsubscribe request, DKIM check or draft has reached a real mailbox from heo; " +
			"its conversation state (archive, move, read, flag, trash and their undo) is previewed, queued and held in process against its declared limits, so no state change has reached a real mailbox from heo either",
		Needs: "a Google Workspace test mailbox, an OAuth client and a Pub/Sub topic; recording `conversation tag` also needs the ID of a label of that mailbox and a message the peer wrote in it, `message reply-draft` a message the peer wrote, `conversation unsubscribe` a signed bulk message with a List-Unsubscribe header, and the state verbs a conversation in the mailbox",
		Tests: append(declared("TestRuntimeDirectMessageChannels"), behavior(gmailHistory)...),
	},
	model.TypeMicrosoftMail: {
		Against: RealSandbox,
		Summary: "only `history fetch` is replayed, from the answers uma recorded from a private mailbox (a window of three messages, one with an attachment, read two at a time with a cursor), " +
			"through the daemon; not recorded by heo, the attachment's download is served by the test, and sending, receiving, a conversation's sender and the other error answers are not exercised; its notification route answering the validation token is tested; " +
			"its own extension_execute effects are run by `conversation tag --set` (`microsoft.mail.categories`), `message reply-draft` (`microsoft.mail.reply_draft`) " +
			"and `conversation unsubscribe` (`microsoft.mail.unsubscribe`): previewed and queued in process with the adapter's own validation, nothing sent, so not recorded; " +
			"no unsubscribe request, DKIM check or draft has reached a real mailbox from heo; " +
			"its conversation state (archive, move, read, flag, trash and their undo) is previewed, queued and held in process against its declared limits, so no state change has reached a real mailbox from heo either",
		Needs: "a Microsoft 365 test mailbox and an app registration; recording `conversation tag` also needs a category of that mailbox's master list and a message the peer wrote in it, `message reply-draft` a message the peer wrote, `conversation unsubscribe` a bulk message with a List-Unsubscribe header, and the state verbs a conversation in the mailbox",
		Tests: append(declared("TestRuntimeDirectMessageChannels"), behavior(graphHistory)...),
	},
}
