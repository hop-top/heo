// Package refdoc renders heo's command reference and cheatsheets from
// the capability manifest (`heo spec`) and the facts the manifest does
// not carry: who may run a verb and which uma call backs it.
//
// The metadata here is the one source for those two facts. A test in
// package cmd pins it to the command tree in both directions: a verb
// without a row, and a row for a verb the tree no longer has, both
// fail. Everything the docs say about a verb that the code already
// knows (side effect, idempotency, dry-run, where it runs, examples)
// comes from the manifest and the tree, never from this file.
package refdoc

import "sort"

// Roles that a Meta may name, in the order the docs print them. They
// are the authorization roles, plus two markers for verbs no role gates.
const (
	Operator = "operator"
	Desk     = "desk"
	Viewer   = "viewer"
	External = "external"

	// Offline is a verb that runs in the invoking process with no
	// daemon, store or role: it reads the binary or the profile files.
	Offline = "none (offline)"
	// Host is a verb of the daemon's own lifecycle or store: the user
	// who runs the daemon, not a granted role.
	Host = "host user"
)

// Meta is what the manifest does not say about one verb.
type Meta struct {
	// Roles is who may run it: authorization roles, or Offline or Host.
	Roles string
	// UMA is the uma call that backs it, or what backs it when uma
	// does not.
	UMA string
	// Limit names where a served surface may not run it, when heo
	// knows it from the command's annotations; empty means heo
	// declares no limit.
	Limit string
}

const (
	anyReader = "desk, viewer, external"
	reader    = "desk, viewer"
	deskOp    = "desk, operator"
)

// verbs is keyed by the verb's path below the root, space separated.
var verbs = map[string]Meta{
	"adapter get":  {Offline, "none: this binary's adapter registry", ""},
	"adapter list": {Offline, "none: this binary's adapter registry", ""},

	"attachment download": {anyReader, "Attachments.Open", ""},
	"attachment get":      {anyReader, "Attachments.Open (metadata only)", ""},
	"attachment list":     {anyReader, "Messages.List (attachment parts)", ""},

	"channel authorize": {Operator, "none: kit's PKCE loopback login, the credential backend; channel get", ""},
	"channel check":     {Operator, "Configuration.ChannelByKey, adapter validation; --remote opens the adapter", ""},
	"channel create":    {Operator, "Configuration.ConfigureChannel", ""},
	"channel get":       {Operator, "Configuration.ChannelByKey", ""},
	"channel list":      {Operator, "Configuration.Channels", ""},
	"channel update":    {Operator, "Configuration.ConfigureChannel (with expected revision)", ""},

	"config get":      {Offline, "none: kit configuration layers", ""},
	"config list":     {Offline, "none: kit configuration layers", ""},
	"config path":     {Offline, "none: kit configuration layers", ""},
	"config paths":    {Offline, "none: kit configuration layers", ""},
	"config set":      {Offline, "none: writes the profile file", ""},
	"config validate": {Offline, "none: settings and adapter validation; channel check through a running daemon", ""},

	"daemon ping": {"any granted role", "none: the daemon's runtime state", ""},

	"contact create": {Desk, "Contacts.Create", ""},
	"contact get":    {reader, "Contacts.List, Contacts.Identities", ""},
	"contact list":   {reader, "Contacts.List", ""},
	"contact merge":  {"desk (operator above asserted assurance)", "Contacts.PreviewMerge, Contacts.Merge", ""},
	"contact update": {Desk, "Contacts.Update", ""},

	"conversation get":         {anyReader, "Conversations.List (scanned for the record ID); WorkItems.List (its work item); desk: Conversations.Capabilities, Conversations.ReadMarker", ""},
	"conversation assign":      {Desk, "Conversations.Assign", ""},
	"conversation label":       {Desk, "Conversations.AddLabels, Conversations.RemoveLabels", ""},
	"conversation link":        {Desk, "Conversations.Link", ""},
	"conversation list":        {anyReader, "Conversations.List (uma filters; workflow: desk, viewer); WorkItems.List per work item (heo filters)", ""},
	"conversation mark-read":   {Desk, "Conversations.MarkRead, Conversations.ReadMarker", ""},
	"conversation mark-unread": {Desk, "Conversations.MarkUnread, Conversations.ReadMarker", ""},
	"conversation reopen":      {Desk, "Conversations.Transition", ""},
	"conversation resolve":     {Desk, "Conversations.Transition", ""},
	"conversation snooze":      {Desk, "Conversations.Snooze", ""},
	"conversation status":      {Desk, "WorkItems.Act (adapter's work_item_act; a uma-owned status is local)", ""},
	"conversation summarize":   {reader, "Conversations.List, Messages.List as the caller; then the model", ""},
	"conversation triage":      {reader, "Conversations.List, Messages.List, Labels.List as the caller; then the model", ""},
	"conversation unassign":    {Desk, "Conversations.Unassign", ""},
	"conversation unlink":      {Desk, "Conversations.Unlink", ""},
	"conversation unsnooze":    {Desk, "Conversations.Unsnooze", ""},
	"conversation tag":         {Desk, "Extensions.Execute (mail labels or categories)", ""},
	"conversation unsubscribe": {Desk, "Extensions.Execute (mail unsubscribe)", ""},
	"conversation archive":     {Desk, "Conversations.Archive, Conversations.Capabilities (preview)", ""},
	"conversation unarchive":   {Desk, "Conversations.Archive (undo), Conversations.Capabilities (preview)", ""},
	"conversation move":        {Desk, "Conversations.Move, Conversations.Capabilities (preview)", ""},
	"conversation read":        {Desk, "Conversations.SetRead, Conversations.Capabilities (preview)", ""},
	"conversation unread":      {Desk, "Conversations.SetRead (unread), Conversations.Capabilities (preview)", ""},
	"conversation flag":        {Desk, "Conversations.Flag, Conversations.Capabilities (preview)", ""},
	"conversation unflag":      {Desk, "Conversations.Flag (undo), Conversations.Capabilities (preview)", ""},
	"conversation trash":       {Desk, "Conversations.Trash, Conversations.Capabilities (preview)", ""},
	"conversation untrash":     {Desk, "Conversations.Trash (undo), Conversations.Capabilities (preview)", ""},
	"conversation update":      {Desk, "Extensions.Execute (adapter's update effect), WorkItems.Act (uma-owned fields)", ""},

	"delivery get":       {deskOp, "Operations.Get", ""},
	"delivery list":      {deskOp, "Operations.List (outbound kinds, the caller's own)", ""},
	"delivery reconcile": {deskOp, "Operations.Reconcile", ""},
	"delivery retry":     {Operator, "Operations.Replay (dead_lettered only)", ""},

	"event get":   {anyReader, "Events.Get", ""},
	"event list":  {anyReader, "Events.List", ""},
	"event watch": {anyReader, "Events.Open, Events.Poll, Events.Ack, Events.Close", ""},

	"identity get":    {reader, "Contacts.Identities, Contacts.History", ""},
	"identity link":   {Desk, "Contacts.Link", ""},
	"identity list":   {reader, "Contacts.Identities", ""},
	"identity unlink": {Desk, "Contacts.Unlink", ""},

	"inbox create":  {Operator, "Configuration.ConfigureInbox", ""},
	"inbox disable": {Operator, "Configuration.ConfigureInbox (disabled)", ""},
	"inbox enable":  {Operator, "Configuration.ConfigureInbox (enabled)", ""},
	"inbox get":     {"operator, desk, viewer, external", "Configuration.InboxByKey", ""},
	"inbox list":    {"operator, desk, viewer, external", "Configuration.Inboxes", ""},
	"inbox update":  {Operator, "Configuration.ConfigureInbox (with expected revision)", ""},

	"label create": {Desk, "Labels.Configure", ""},
	"label list":   {reader, "Labels.List", ""},

	"provider-label create": {Desk, "Extensions.Execute (mail mailbox create); Extensions.Query (preview: the name exists)", ""},
	"provider-label list":   {deskOp, "Extensions.Query (mail mailbox list)", ""},
	"message delete":        {Desk, "Messages.Delete", ""},
	"message draft":         {Desk, "Conversations.List, Messages.List as the caller; then the model", ""},
	"message edit":          {Desk, "Messages.Edit", ""},
	"message get":           {anyReader, "Messages.List (scanned for the record ID)", ""},
	"message list":          {anyReader, "Messages.List (text, time and label filters applied by uma)", ""},
	"message raw":           {anyReader, "Messages.List (scanned for the record ID); Messages.Raw", ""},
	"message reply":         {Desk, "Messages.Reply", ""},
	"message reply-draft":   {Desk, "Extensions.Execute (mail reply draft)", ""},
	"message send":          {Desk, "Conversations.Start", ""},

	"note add":  {Desk, "Messages.AddInternalNote", ""},
	"note get":  {Desk, "Messages.List (scanned for the record ID; internal visibility only)", ""},
	"note list": {Desk, "Messages.List (internal visibility only)", ""},

	"operation get":   {deskOp, "Operations.Get; --key uses Operations.Lookup, or Operations.List without --kind and --target", ""},
	"operation list":  {deskOp, "Operations.List (the caller's own)", ""},
	"operation watch": {deskOp, "Operations.Get, polled", ""},

	"operation held list":    {deskOp, "Operations.List (held, as the caller may read it)", ""},
	"operation held show":    {deskOp, "Operations.Get (held block)", ""},
	"operation held approve": {deskOp + " (people only; a selector approves its matches under one batch)", "Operations.Approve", ""},
	"operation held edit":    {deskOp + " (people only)", "Operations.Edit", ""},
	"operation held reject":  {deskOp + " (people only)", "Operations.Reject", ""},
	"operation held history": {deskOp, "Operations.Get or Operations.List (held, derived), Events.List (operation.held)", ""},
	"operation trust list":   {deskOp, "Operations.Trusts", ""},
	"operation trust import": {Operator, "Operations.ImportTrust (trust.write); Operations.Trust as the runtime", ""},
	"operation trust set":    {deskOp + " (people only)", "Operations.Trust, ResetTrust and ImportTrust, as the runtime", ""},

	"history fetch": {"desk, operator (channel)", "Runtime.Import (requester holds channel.import)", ""},

	"participant add":    {Desk, "Conversations.AddParticipant", ""},
	"participant list":   {anyReader, "Conversations.List, Contacts.Identities", ""},
	"participant remove": {Desk, "Conversations.RemoveParticipant", ""},

	"reaction add":    {Desk, "Messages.React", ""},
	"reaction list":   {anyReader, "Messages.List (scanned for the record ID; stored reactions)", ""},
	"reaction remove": {Desk, "Messages.React", ""},

	"interaction get":     {reader, "Interactions.List (scanned for the record ID)", ""},
	"interaction list":    {reader, "Interactions.List", ""},
	"interaction respond": {Desk, "Interactions.Respond", ""},

	"interaction open-modal":   {Desk, "Extensions.Execute (slack views.open)", ""},
	"interaction push-modal":   {Desk, "Extensions.Execute (slack views.push)", ""},
	"interaction update-modal": {Desk, "Extensions.Execute (slack views.update)", ""},

	"receipt send": {Desk, "Messages.Receipt", ""},

	"hook list":              {Operator, "none: the hooks service's runners and dead-letter store", ""},
	"hook test":              {Operator, "Events.Get as the plugin; runs the plugin, applies nothing", ""},
	"hook replay":            {Operator, "Events.List as the plugin; queues a job that runs the plugin and applies its operations", ""},
	"hook dead-letter retry": {Operator, "Events.Get as the plugin; queues a job that runs the plugin and applies its operations", ""},
	"hook job get":           {Operator, "none: the hooks service's in-memory jobs", ""},
	"hook dead-letter list":  {Operator, "none: the hooks service's dead-letter store", ""},
	"hook dead-letter drop":  {Operator, "none: the hooks service's dead-letter store", ""},

	"plugin token mint":   {Host, "none: credential store", ""},
	"plugin token revoke": {Host, "none: credential store", ""},

	"service list":    {Host, "none: kit service registry", "socket only (management)"},
	"service restart": {Host, "none: kit control socket", ""},
	"service setup":   {Host, "none: profile file and credential store", ""},
	"service start":   {Host, "none: starts the daemon", ""},
	"service status":  {Host, "none: kit control socket and heo sections", "socket only (management)"},
	"service stop":    {Host, "none: kit control socket", ""},

	"spec":          {Offline, "none: kit toolspec", ""},
	"spec coverage": {Offline, "none: kit toolspec", ""},
	"status":        {Offline, "none: kit tool status", ""},

	"storage backup":  {Host, "none: storage driver (takes store ownership)", ""},
	"storage migrate": {Host, "none: storage driver (takes store ownership)", ""},
	"storage restore": {Host, "none: storage driver (takes store ownership)", ""},
	"storage status":  {Host, "none: storage driver", ""},

	"task cancel": {Desk, "Tasks.Act", ""},
	"task get":    {reader, "Tasks.Get", ""},
	"task list":   {reader, "Tasks.List", ""},
	"task watch":  {Desk, "Events.Open, Events.Poll, Events.Ack, Events.Close", ""},

	"token claims": {Offline, "none: kit authentication", ""},
	"token decode": {Offline, "none: kit authentication", ""},
	"token verify": {Offline, "none: kit authentication", ""},

	"upgrade": {Host, "none: kit upgrade", ""},
}

// Lookup returns the metadata of the verb at path (below the root).
func Lookup(path string) (Meta, bool) {
	m, ok := verbs[path]
	return m, ok
}

// Paths lists every verb with a metadata row, sorted.
func Paths() []string {
	out := make([]string, 0, len(verbs))
	for p := range verbs {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Status of a verb that the grammar names and heo does not have.
const (
	// Deferred waits on a uma capability that does not exist yet (no call
	// for it, or no field to hold it).
	Deferred = "deferred"
	// NotBuilt is heo-side work that has not been done.
	NotBuilt = "not built"
	// Cut was decided against; the reason names what replaces it.
	Cut = "cut"
	// Folded is served by another verb.
	Folded = "folded"
)

// Gap is a verb (or feature) of the grammar that is not in the command
// tree, and why.
type Gap struct {
	// Verb is the path below the root, or a short feature name in
	// parentheses.
	Verb   string
	Status string
	// Why is the blocker, or what replaces the verb.
	Why string
}

// Gaps is every verb the grammar catalog names that the command tree
// does not carry. A test fails when a row names a verb the tree has
// (implemented since: delete the row) and when a catalog verb is in
// neither the tree nor here.
var Gaps = []Gap{
	{"message search", Folded, "`message list --text`, `--since`, `--until` and the label filters search; uma applies them"},
	{"channel enable", Cut, "availability is the inbox's: `inbox enable` and `inbox disable`"},
	{"channel disable", Cut, "availability is the inbox's: `inbox enable` and `inbox disable`"},
	{"(per-inbox grants)", Deferred, "after v0.1: a grant names an actor and roles for the whole tenant, never one inbox"},
}

// Catalog is every verb the grammar names, implemented or not. A test
// holds each one to be in the command tree or in Gaps, so a verb
// cannot be dropped from the docs unrecorded.
var Catalog = []string{
	"adapter list", "adapter get",
	"channel list", "channel get", "channel create", "channel update", "channel check", "channel authorize",
	"channel enable", "channel disable",
	"inbox list", "inbox get", "inbox create", "inbox update", "inbox enable", "inbox disable",
	"contact list", "contact get", "contact create", "contact update", "contact merge",
	"identity list", "identity get", "identity link", "identity unlink",
	"conversation list", "conversation get", "conversation label", "conversation tag", "conversation unsubscribe", "conversation resolve",
	"conversation reopen", "conversation archive", "conversation unarchive",
	"conversation move", "conversation read", "conversation unread", "conversation flag", "conversation unflag",
	"conversation trash", "conversation untrash",
	"conversation mark-read", "conversation mark-unread", "conversation assign",
	"conversation unassign", "conversation snooze", "conversation unsnooze",
	"conversation link", "conversation unlink", "conversation status", "conversation update",
	"conversation summarize", "conversation triage", "message draft",
	"label create", "label list", "provider-label list", "provider-label create",
	"message list", "message get", "message reply", "message reply-draft", "message send", "message search",
	"message edit", "message delete", "message raw",
	"note list", "note get", "note add",
	"attachment list", "attachment get", "attachment download",
	"operation list", "operation get", "operation watch",
	"operation held list", "operation held show", "operation held approve", "operation held edit", "operation held reject",
	"operation held history", "operation trust list", "operation trust import", "operation trust set",
	"delivery list", "delivery get", "delivery retry", "delivery reconcile",
	"event list", "event get", "event watch",
	"reaction list", "reaction add", "reaction remove",
	"participant list", "participant add", "participant remove",
	"receipt send", "history fetch",
	"interaction list", "interaction get", "interaction respond",
	"interaction open-modal", "interaction push-modal", "interaction update-modal",
	"task list", "task get", "task watch", "task cancel",
	"service start", "service status", "service stop", "service list", "service setup",
	"plugin token mint", "plugin token revoke",
	"hook list", "hook test", "hook replay", "hook job get", "hook dead-letter list", "hook dead-letter retry", "hook dead-letter drop",
	"status", "upgrade",
	"storage status", "storage backup", "storage restore", "storage migrate",
	"config get", "config list", "config set", "config validate", "config path", "config paths",
}
