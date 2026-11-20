//go:build heoe2e

package cmd

import (
	"os"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
	"hop.top/uma/adapter/ticket/github"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
)

// Environment the e2e build's fake adapter reads, so a test can stall
// a built daemon's delivery and see what its sends did.
const (
	// e2eSendDelayEnv holds every fake send this long (a Go duration).
	e2eSendDelayEnv = "HEO_E2E_FAKE_SEND_DELAY"
	// e2eSendLogEnv names the file fake sends record their progress in.
	e2eSendLogEnv = "HEO_E2E_FAKE_SEND_LOG"
	// e2eReconcileEnv has the fake adapter declare reconcile, as an
	// adapter that can look a sent message up does, and says what its
	// lookup answers: "1" that the provider cannot be reached, "found"
	// that it holds the message, "missing" that it holds no record of it
	// (with no settle window), "unproven" that it holds none inside a
	// settle window an hour long, which proves nothing.
	e2eReconcileEnv = "HEO_E2E_FAKE_RECONCILE"
	// e2eEffectLogEnv names the file the effects fake records the
	// edits, deletes and reactions it applies in, the acts fake the
	// work item acts, and the verbs fake the receipts and membership
	// changes.
	e2eEffectLogEnv = "HEO_E2E_FAKE_EFFECT_LOG"
	// e2eRouteLogEnv names the file the base fake records the routes
	// its sends are asked to send to in.
	e2eRouteLogEnv = "HEO_E2E_FAKE_ROUTE_LOG"
)

// extraAdapters registers the fake adapter in an e2e build, so the
// built daemon can be driven with channels end to end. Release builds
// never carry it.
func extraAdapters() []adapters.Adapter {
	f := adapterstest.New()
	if d, err := time.ParseDuration(os.Getenv(e2eSendDelayEnv)); err == nil && d > 0 {
		f.DelaySends(d)
	}
	if p := os.Getenv(e2eSendLogEnv); p != "" {
		f.LogSends(p)
	}
	if p := os.Getenv(e2eRouteLogEnv); p != "" {
		f.LogRoutes(p)
	}
	switch os.Getenv(e2eReconcileEnv) {
	case "1":
		f.EnableReconcile()
	case "found":
		f.EnableReconcile()
		f.ReconcileFinds()
	case "missing":
		f.EnableReconcile()
		f.ReconcileMisses()
	case "unproven":
		f.EnableReconcile()
		f.ReconcileMisses()
		f.ReconcileSettle(time.Hour)
	}
	// The second fake declares edit, delete and react beside the first,
	// which does not: a test holds a channel of each.
	fx := adapterstest.New()
	fx.As(adapterstest.EffectsType)
	fx.DeclareEffects()
	if p := os.Getenv(e2eEffectLogEnv); p != "" {
		fx.LogEffects(p)
	}
	// The third declares work_item_act with the issue tracker's open and
	// closed acts, and records the acts it applies in the same log.
	fa := adapterstest.New()
	fa.As(adapterstest.ActsType)
	fa.DeclareWorkItemActs()
	if p := os.Getenv(e2eEffectLogEnv); p != "" {
		fa.LogEffects(p)
	}
	// The fourth declares membership, receipt and history: it applies
	// participant changes and receipts, recording them in the same
	// log, and serves the history a test posts to a channel's webhook
	// (adapterstest.HistoryRoute).
	fv := adapterstest.New()
	fv.As(adapterstest.VerbsType)
	fv.DeclareMembership()
	fv.DeclareReceipts()
	fv.DeclareHistory()
	if p := os.Getenv(e2eEffectLogEnv); p != "" {
		fv.LogEffects(p)
	}
	// The fifth reports interactions, as a chat provider that is only a
	// receiver does, and declares no respond: heo reads what it reports
	// and cannot answer it.
	fi := adapterstest.New()
	fi.As(adapterstest.InteractionType)
	fi.DeclareInteractions()
	// The extension-effect variants each declare extension_execute and
	// ship the effect of another adapter, under its uma definition, and
	// record the effects they run in the same log: GitHub's issue update,
	// Gmail's labels, Outlook's categories, and Zendesk's ticket update
	// alone (the capability with none of the others).
	var variants []adapters.Adapter
	for _, v := range []struct {
		typ    model.TypeKey
		effect string
	}{
		{adapterstest.UpdatesType, github.ExtensionUpdate},
		{adapterstest.LabelsType, google.ExtensionLabels},
		{adapterstest.CategoriesType, microsoft.ExtensionCategories},
		{adapterstest.DraftsType, google.ExtensionReplyDraft},
		{adapterstest.UnsubscribeType, google.ExtensionUnsubscribe},
		{adapterstest.ForwardType, google.ExtensionForward},
		{adapterstest.TicketEffectType, zendesk.ExtensionUpdate},
	} {
		fe := adapterstest.New()
		fe.As(v.typ)
		fe.DeclareEffect(v.effect)
		if p := os.Getenv(e2eEffectLogEnv); p != "" {
			fe.LogEffects(p)
		}
		variants = append(variants, fe.Adapter())
	}
	// The state variants declare provider conversation state as a mailbox
	// does (account scope, label moves, every flag kind and role) and as
	// a folder-shaped shared provider does (archive, move to the inbox
	// and archive roles, the star flag), and record each change in the
	// same log.
	fs := adapterstest.New()
	fs.As(adapterstest.StateType)
	fs.DeclareState(adapterstest.StateOptions{})
	fsf := adapterstest.New()
	fsf.As(adapterstest.StateFolderType)
	fsf.DeclareState(adapterstest.StateOptions{
		Caps:  []model.Capability{model.CapabilityArchive, model.CapabilityMove, model.CapabilityFlag},
		Scope: model.ScopeShared, Shape: model.MoveShapeFolder,
		Kinds: []string{model.FlagStar}, Roles: []string{model.RoleInbox, model.RoleArchive},
	})
	if p := os.Getenv(e2eEffectLogEnv); p != "" {
		fs.LogEffects(p)
		fsf.LogEffects(p)
	}
	variants = append(variants, fs.Adapter(), fsf.Adapter())
	// The mailbox variant declares raw_read and the provider mailbox list
	// and create of its own type over a small mailbox list, the state
	// capabilities and Gmail's label change, as a Gmail mailbox does, and
	// records each create and change in the same log.
	fb := adapterstest.New()
	fb.As(adapterstest.MailboxType)
	fb.DeclareRaw(nil)
	fb.DeclareMailboxes(adapterstest.MailboxFixtures()...)
	fb.DeclareState(adapterstest.StateOptions{})
	fb.DeclareEffect(google.ExtensionLabels)
	if p := os.Getenv(e2eEffectLogEnv); p != "" {
		fb.LogEffects(p)
	}
	variants = append(variants, fb.Adapter())
	// The recipients variant declares reply_recipients (reply-all and cc,
	// no bcc, at most four addresses) and ships Gmail's reply draft, and
	// records the recipients each reply carries in the same log.
	fr := adapterstest.New()
	fr.As(adapterstest.RecipientsType)
	fr.DeclareRecipients(adapterstest.RecipientsLimits)
	fr.DeclareEffect(google.ExtensionReplyDraft)
	if p := os.Getenv(e2eEffectLogEnv); p != "" {
		fr.LogEffects(p)
	}
	variants = append(variants, fr.Adapter())
	// The sixth reports interactions the way Slack does and ships Slack's
	// three modal effects: opening, stacking and updating a view. It
	// records each in the same log and names the view an open shows.
	fm := adapterstest.New()
	fm.As(adapterstest.ViewsType)
	fm.DeclareInteractions()
	for _, e := range []string{slack.ExtensionViewsOpen, slack.ExtensionViewsPush, slack.ExtensionViewsUpdate} {
		fm.DeclareEffect(e)
	}
	if p := os.Getenv(e2eEffectLogEnv); p != "" {
		fm.LogEffects(p)
	}
	variants = append(variants, fm.Adapter())
	// uma's A2A adapter, a maintained adapter this build carries so a
	// remote agent can open and cancel tasks over JSON-RPC.
	// The sixth sends over HTTP to a provider stand-in a test owns, so a
	// cassette can record the exchange (internal/xrrx).
	// The normalizing variant implements adapter.RecipientNormalizer, as
	// relay.messages does, and refuses the recipients a test names.
	return append([]adapters.Adapter{f.Adapter(), fx.Adapter(), fa.Adapter(), fv.Adapter(), fi.Adapter(), adapterstest.A2A(), adapterstest.Provider(),
		adapterstest.NormalizerAdapter()}, variants...)
}
