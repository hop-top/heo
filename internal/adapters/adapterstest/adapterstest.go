// Package adapterstest is a scriptable fake uma adapter for heo's tests:
// a provider that receives persistently and over HTTP, sends, probes its
// permissions and names its provider account from channel
// configuration.
//
// It is never registered in a release binary. Tests compose it
// directly, and the e2e build (tag heoe2e) registers it in the
// composition root so the built daemon can be driven end to end.
package adapterstest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
)

// Type is the fake adapter's type key.
const Type model.TypeKey = "test.heo.fake"

// EffectsType is the type key of the fake variant that declares edit,
// delete and react (see Factory.DeclareEffects), registered beside Type
// in the e2e build so a test can hold an adapter that declares them
// next to one that does not.
const EffectsType model.TypeKey = "test.heo.fake.effects"

// VerbsType is the type key of the fake variant that declares
// membership, receipt and history (see Factory.DeclareMembership,
// DeclareReceipts and DeclareHistory), registered beside Type in the
// e2e build.
const VerbsType model.TypeKey = "test.heo.fake.verbs"

// ActsType is the type key of the fake variant that declares
// work_item_act with the transitions of an issue tracker (see
// Factory.DeclareWorkItemActs), registered beside Type in the e2e
// build.
const ActsType model.TypeKey = "test.heo.fake.acts"

// Schema names the fake adapter's channel configuration.
const Schema = "test.heo.fake.config"

// Namespace is the identity namespace of the fake adapter's senders.
const Namespace = "test.heo.fake.user"

// Config is a fake channel's configuration.
type Config struct {
	// Account is the provider account the channel connects to.
	Account string `json:"account"`
	// Broken makes every open of the channel fail permanently.
	Broken bool `json:"broken,omitempty"`
	// Denied lists capabilities the provider's permission probe reports
	// unavailable (model.Unauthorized), as an account lacking a scope
	// would.
	Denied []model.Capability `json:"denied,omitempty"`
	// NoWebhook makes the channel's instance report receive_http
	// unavailable, as a mailbox with push turned off does: the channel
	// declares it, yet has no webhook to mount.
	NoWebhook bool `json:"no_webhook,omitempty"`
	// Disabled lists capabilities the channel's instance reports
	// unavailable with reason disabled, as an adapter whose own
	// configuration switches a capability off does (github.issues
	// without replies): declared, yet not usable.
	Disabled []model.Capability `json:"disabled,omitempty"`
	// Withdrawn lists capabilities the channel's instance reports
	// unavailable with reason unauthorized, as an adapter whose account
	// lost a permission does: the report uma's attempt gate reads, where
	// Denied is the permission probe only the capability view merges.
	Withdrawn []model.Capability `json:"withdrawn,omitempty"`
	// Link makes the channel's receiver report its link, as the Discord
	// gateway does: "connected" reports it up, "connecting" never does.
	// Empty reports nothing.
	Link string `json:"link,omitempty"`
	// FailReceive makes every receiver connection fail with this error
	// code, and RetryAfter, a Go duration, is the wait the failure
	// states.
	FailReceive model.ErrorCode `json:"fail_receive,omitempty"`
	RetryAfter  string          `json:"retry_after,omitempty"`
}

// RevisionHeader is the response header naming the channel revision
// whose instance answered a webhook request.
const RevisionHeader = "X-Fake-Revision"

// StreamRoute is the webhook path, relative to the channel's route,
// that answers 200 and then holds the response open until the request
// ends, as a browser event stream does.
const StreamRoute = "/stream"

// InjectRoute is the webhook path, relative to the channel's route,
// where a test hands the channel an inbound message, as a provider's
// push would: POST an Inject, answered 200 with the receipt's
// duplicate flag, or the sink's refusal.
const InjectRoute = "/inject"

// Inject is one inbound envelope a test has the channel accept.
type Inject struct {
	// Event and Message identify the envelope and its provider message.
	Event   string `json:"event"`
	Message string `json:"message"`
	// Kind defaults to a new message; EnvelopeMessageEdited edits the
	// message Message names.
	Kind string `json:"kind,omitempty"`
	// From is the sender's ID in the fake's identity namespace.
	From string `json:"from"`
	// Parent is the provider message the sender wrote, as the parent of
	// the sender's route, which a conversation's reply route is: a mail
	// provider's reply route names it. None when empty.
	Parent string `json:"parent,omitempty"`
	// InReplyTo is the provider message this one replies to (a thread
	// or quote reply); uma resolves it when it holds that message in
	// the conversation. None when empty.
	InReplyTo string `json:"in_reply_to,omitempty"`
	// EmailMessageID, EmailReferences and EmailInReplyTo are the mail's
	// own threading headers ("<left@right>"), reported as an email
	// adapter reports them (model.EmailHeaders), so a test can have a
	// forwarded copy arrive with the original's Message-ID in its
	// References. None when empty.
	EmailMessageID  string   `json:"email_message_id,omitempty"`
	EmailReferences []string `json:"email_references,omitempty"`
	EmailInReplyTo  string   `json:"email_in_reply_to,omitempty"`
	// Destination is the inbox destination the envelope is for.
	Destination string `json:"destination"`
	// Text is the envelope's text part, none when empty.
	Text string `json:"text,omitempty"`
	// Audience defaults to public. The attachments below are staged
	// under it, as uma requires of an envelope's attachments.
	Audience model.Audience `json:"audience,omitzero"`
	// OccurredAt defaults to now.
	OccurredAt time.Time `json:"occurred_at,omitzero"`
	// Attachments are staged through the sink and attached.
	Attachments []InjectAttachment `json:"attachments,omitempty"`
	// Issue, when set, makes the envelope report the provider's state of
	// an issue, as a GitHub webhook would. With Kind EnvelopeWorkItem it
	// carries no message; with the default kind it is the issue's first
	// comment.
	Issue *InjectIssue `json:"issue,omitempty"`
	// Room names the provider room the message was posted in, as a room
	// reference on the envelope, which a per-room inbox requires.
	Room string `json:"room,omitempty"`
	// Thread names the provider thread the message belongs to, as a
	// thread reference whose parent is Room, which a per-thread inbox
	// requires.
	Thread string `json:"thread,omitempty"`
	// AgentContext names the agent context the message belongs to, as
	// an agent protocol adapter's would: the envelope carries a
	// context reference, which a per-agent-context inbox requires.
	AgentContext string `json:"agent_context,omitempty"`
	// OpenTask asks the engine to open an agent task for the message
	// (an envelope task binding). Only a per-agent-context inbox
	// accepts it.
	OpenTask bool `json:"open_task,omitempty"`
	// Interaction, when set, makes the envelope report an interaction
	// its sender performed instead of a message: Event is the
	// provider's interaction ID, and Message, Text, attachments and
	// Issue are not used.
	Interaction *InjectInteraction `json:"interaction,omitempty"`
}

// InjectIssue is the provider's view of one issue. The fake reports it
// in the GitHub adapter's namespace, so heo classifies it as an issue.
type InjectIssue struct {
	// Parent is the repository, "owner/name"; empty is the envelope's
	// destination.
	Parent string `json:"parent,omitempty"`
	// Number identifies the issue in its repository.
	Number  string `json:"number"`
	Subject string `json:"subject,omitempty"`
	// Status is uma's normalized status (open, closed); RawStatus the
	// provider's own, such as closed:completed.
	Status    string `json:"status"`
	RawStatus string `json:"raw_status,omitempty"`
	// Labels are the issue's label names, Assignees its assignees' IDs
	// and Milestone its milestone, as GitHub reports them.
	Labels    []string         `json:"labels,omitempty"`
	Assignees []string         `json:"assignees,omitempty"`
	Milestone *InjectMilestone `json:"milestone,omitempty"`
}

// InjectMilestone is an issue's milestone.
type InjectMilestone struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

// InjectAttachment is inbound media: its bytes are staged through the
// sink before the envelope names them.
type InjectAttachment struct {
	// Key identifies the media at the provider.
	Key       string `json:"key"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
}

// envelope stages in's attachments and builds its envelope.
func (i *instance) envelope(ctx context.Context, sink adapter.Sink, in Inject) (model.Envelope, error) {
	if in.Kind == "" {
		in.Kind = model.EnvelopeMessage
	}
	if in.Audience.Visibility == "" {
		in.Audience = model.Audience{Visibility: model.Public}
	}
	if in.OccurredAt.IsZero() {
		in.OccurredAt = time.Now().UTC()
	}
	env := model.Envelope{
		EventID: in.Event, MessageID: in.Message, Destination: in.Destination, Kind: in.Kind,
		Sender:    model.ExternalRef{Channel: i.ch.Ref, Namespace: Namespace, Kind: "user", ID: in.From, ParentID: in.Parent},
		Assurance: "verified", Audience: in.Audience, OccurredAt: in.OccurredAt,
	}
	if in.Kind == model.EnvelopeMessageEdited {
		env.SourceRevision = in.Event
	} else {
		env.InReplyTo = in.InReplyTo
	}
	if in.Issue != nil {
		parent := in.Issue.Parent
		if parent == "" {
			parent = in.Destination
		}
		ref := model.ExternalRef{Channel: i.ch.Ref, Namespace: github.Namespace, Kind: model.RefKindWorkItem, ID: in.Issue.Number, ParentID: parent}
		raw := in.Issue.RawStatus
		if raw == "" {
			raw = in.Issue.Status
		}
		env.References = append(env.References, ref)
		env.SourceRevision = in.Event
		env.WorkItem = &model.WorkItemState{
			Subject: in.Issue.Subject, RawStatus: raw, Status: in.Issue.Status, Binding: model.WorkItemBindingActive,
		}
		for _, a := range in.Issue.Assignees {
			env.WorkItem.Assignees = append(env.WorkItem.Assignees, model.ExternalRef{Channel: i.ch.Ref, Namespace: github.Namespace, Kind: github.RefKindUser, ID: a})
		}
		if len(in.Issue.Labels) > 0 {
			data, _ := json.Marshal(in.Issue.Labels)
			env.WorkItem.Fields = append(env.WorkItem.Fields, model.Extension{Name: github.FieldLabels, Version: 1, Data: data})
		}
		if m := in.Issue.Milestone; m != nil {
			data, _ := json.Marshal(m)
			env.WorkItem.Fields = append(env.WorkItem.Fields, model.Extension{Name: github.FieldMilestone, Version: 1, Data: data})
		}
		if in.Kind == model.EnvelopeWorkItem {
			env.MessageID, env.Sender = "", model.ExternalRef{}
		}
	}
	if in.EmailMessageID != "" || len(in.EmailReferences) > 0 || in.EmailInReplyTo != "" {
		norm := model.EmailHeaders{
			MessageID: in.EmailMessageID, InReplyTo: in.EmailInReplyTo, References: strings.Join(in.EmailReferences, " "),
		}.Normalize(i.ch.Ref)
		env.References = append(env.References, norm.References...)
	}
	if in.Room != "" {
		env.References = append(env.References, model.ExternalRef{
			Channel: i.ch.Ref, Namespace: Namespace, Kind: model.RefKindRoom, ID: in.Room,
		})
	}
	if in.Thread != "" {
		env.References = append(env.References, model.ExternalRef{
			Channel: i.ch.Ref, Namespace: Namespace, Kind: model.RefKindThread, ID: in.Thread, ParentID: in.Room,
		})
	}
	if in.AgentContext != "" {
		env.References = append(env.References, model.ExternalRef{
			Channel: i.ch.Ref, Namespace: Namespace, Kind: model.RefKindContext, ID: in.AgentContext,
		})
	}
	if in.OpenTask {
		env.Task = &model.TaskBinding{Open: true}
	}
	if in.Interaction != nil {
		i.fillInteraction(&env, in)
		return env, nil
	}
	if in.Text != "" {
		env.Content.Parts = append(env.Content.Parts, model.Part{Kind: model.PartText, Text: in.Text})
	}
	for _, a := range in.Attachments {
		stager, ok := sink.(adapter.AttachmentSink)
		if !ok {
			return env, errors.New("sink stages no attachments")
		}
		ref, err := stager.StageAttachment(ctx, adapter.StageRequest{
			Key: a.Key, Destination: in.Destination,
			Metadata: model.AttachmentMetadata{Name: a.Name, MediaType: a.MediaType, Size: int64(len(a.Data)), Audience: in.Audience},
		}, bytes.NewReader(a.Data))
		if err != nil {
			return env, err
		}
		env.Content.Parts = append(env.Content.Parts, model.Part{Kind: model.PartAttachment, Attachment: &ref})
	}
	return env, nil
}

// inject accepts the Inject r carries.
func (i *instance) inject(w http.ResponseWriter, r *http.Request, sink adapter.Sink) {
	var in Inject
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	env, err := i.envelope(r.Context(), sink, in)
	if err == nil {
		var receipt model.IngressReceipt
		receipt, err = sink.Accept(r.Context(), model.Ingress{Call: model.Call{Correlation: "fake-" + model.CorrelationID(in.Event)}, Envelope: env})
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]bool{"duplicate": receipt.Duplicate})
			return
		}
	}
	http.Error(w, err.Error(), http.StatusUnprocessableEntity)
}

// Configuration returns c as a channel configuration extension.
func (c Config) Configuration() model.Extension {
	data, _ := json.Marshal(c)
	return model.Extension{Name: Schema, Version: 1, Data: data}
}

// Factory is the fake adapter. The zero value is unusable; use New.
type Factory struct {
	gate chan struct{}

	mu          sync.Mutex
	receiving   map[model.ID]int
	runs        map[model.ID]int
	sending     chan adapter.SendRequest
	interrupted int

	// serving, when set by ServeAfter, gets each receiver's report that
	// it serves, serveAfter into its run.
	serving    *adapters.Serving
	serveAfter time.Duration
	served     map[model.ID]bool

	// script, set by ScriptReceive, runs each receiver connection;
	// reports, set by ReportLink, makes the instances report their link
	// (receiver.go).
	script  func(context.Context, Connection) error
	reports bool
	// staggered, set by Stagger, marks the adapter Staggered.
	staggered bool
	// openDelay, set by SlowOpen, holds every open of an instance.
	openDelay time.Duration

	// notes, set by DeliverNotes, declares internal_note.
	notes bool

	// interactions, set by DeclareInteractions, declares
	// interaction.received (interactions.go).
	interactions bool

	// effects, set by DeclareEffects, declares edit, delete and react;
	// effectLog records each applied one; typ, set by As, replaces Type.
	effects    bool
	effectLog  chan Effect
	effectFile string
	typ        model.TypeKey

	// state, set by DeclareState, declares the provider conversation
	// state capabilities (state.go).
	state *StateOptions

	// raw, set by DeclareRaw, declares raw_read (raw.go); boxes, set by
	// DeclareMailboxes, declares the mailbox list and create
	// extensions (mailboxes.go).
	raw   func(model.ExternalRef) ([]byte, error)
	boxes *mailboxState

	// recipients, set by DeclareRecipients, declares reply_recipients
	// with these limits (recipients.go).
	recipients *model.RecipientLimits

	// members, receipts and history, set by DeclareMembership,
	// DeclareReceipts and DeclareHistory, declare membership, receipt
	// and history; verbs.go applies them.
	members, receipts, history bool
	held                       map[model.ID]*held
	historyReads               []adapter.HistoryRequest

	// acts, set by DeclareWorkItemActs, declares work_item_act with the
	// transitions in actSet; actLog receives each act the provider
	// applied.
	// shipped, set by DeclareEffect, are the extension effects f declares
	// extension_execute for and runs (extensions.go).
	shipped []adapter.ExtensionDefinition

	acts   bool
	actSet []adapter.WorkItemTransition
	actLog chan Act

	// sendDelay, set by DelaySends, holds each send before it
	// completes; sendLog, set by LogSends, is the file each send
	// records its progress in.
	sendDelay time.Duration
	sendLog   string

	// routeLog, set by LogRoutes, is the file each send records the
	// route it is asked to send to in.
	routeLog string

	// stall, set by Stall, makes capability checks block until their
	// context ends; sendErr, set by FailSends, fails every send.
	stall   bool
	stalled chan struct{}
	sendErr error

	// reconcile, set by EnableReconcile, declares reconcile; recon is
	// what a reconciliation answers, set by ReconcileAs, and reconciles
	// counts them.
	reconcile  bool
	reconMode  string
	settle     time.Duration
	reconOut   adapter.Outcome
	reconErr   error
	reconciles int

	// reconGate, set by HoldReconciles, blocks each lookup until
	// ReleaseReconciles; reconciling gets a signal as each lookup starts.
	reconGate   chan struct{}
	reconciling chan struct{}
}

// New returns a fake factory whose sends complete at once.
func New() *Factory {
	f := &Factory{
		receiving: map[model.ID]int{}, runs: map[model.ID]int{}, sending: make(chan adapter.SendRequest, 16),
		served: map[model.ID]bool{}, held: map[model.ID]*held{}, stalled: make(chan struct{}, 16), reconciling: make(chan struct{}, 16), effectLog: make(chan Effect, 64), actLog: make(chan Act, 64),
	}
	f.gate = make(chan struct{})
	close(f.gate)
	return f
}

// Hold makes later sends block until Release, so a test can stop the
// daemon while a send is in flight.
func (f *Factory) Hold() {
	f.mu.Lock()
	f.gate = make(chan struct{})
	f.mu.Unlock()
}

// Release lets held sends complete.
func (f *Factory) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.gate:
	default:
		close(f.gate)
	}
}

// DelaySends makes every send take d before it completes, unless its
// context ends first: a delivery a built daemon can be stopped during.
func (f *Factory) DelaySends(d time.Duration) {
	f.mu.Lock()
	f.sendDelay = d
	f.mu.Unlock()
}

// LogSends makes every send append its progress to the file at path,
// one line per step: "start <id>" as it begins, then "done <id>",
// "failed <id>" or "interrupted <id>", id being the send's operation ID. A test
// driving a built daemon reads what its sends did there.
func (f *Factory) LogSends(path string) {
	f.mu.Lock()
	f.sendLog = path
	f.mu.Unlock()
}

// LogRoutes makes every send append the route it is asked to send to
// to the file at path: "<id> <kind> <parent> <route id>", id being the
// send's operation ID and parent "-" when the route has none. A test
// driving a built daemon reads where the provider was asked to send
// there.
func (f *Factory) LogRoutes(path string) {
	f.mu.Lock()
	f.routeLog = path
	f.mu.Unlock()
}

// logSend appends one step of a send to path, when set.
func logSend(path, step string, id model.ID) {
	if path == "" {
		return
	}
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // test-chosen path
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(fh, "%s %s\n", step, id)
	_ = fh.Close()
}

// Stall, on, makes the capability checks of f's instances block until
// their context ends, as a provider that never answers would; off ends
// it. A worker checks before it records an attempt, so an operation
// whose claims all stall ends dead-lettered with nothing sent. Turn it
// on after submitting: a submission checks capabilities too.
func (f *Factory) Stall(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stall = on
}

// Stalled receives once for each capability check that starts to
// stall.
func (f *Factory) Stalled() <-chan struct{} { return f.stalled }

// FailSends makes later sends fail with err: a typed uma error is the
// provider's answer, anything else leaves the outcome unknown.
func (f *Factory) FailSends(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr = err
}

// ServeAfter makes f a listener-serving adapter: each receiver reports
// that it serves d into its run, as a receiver binding a slow listener
// would. Call it before Adapter.
func (f *Factory) ServeAfter(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serving = adapters.NewServing()
	f.serveAfter = d
}

// Served reports whether a receiver of channel id has reported that it
// serves.
func (f *Factory) Served(id model.ID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.served[id]
}

// DeliverNotes makes f declare internal_note and deliver notes as
// private provider notes, so an inbox on it may deliver notes. Call it
// before the engine starts.
func (f *Factory) DeliverNotes() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = true
}

// DeclareEffects makes f declare edit, delete and react, and apply
// them: each attempt is accepted by the provider and recorded (see
// Effects). Call it before the engine starts.
func (f *Factory) DeclareEffects() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.effects = true
}

// LogEffects makes every applied edit, delete or reaction append one
// line to the file at path: "edit <target> <text>", "delete <target>",
// "react <target> <emoji>" or "unreact <target> <emoji>", the target
// being the provider message ID. A test driving a built daemon reads
// what the provider was asked there.
func (f *Factory) LogEffects(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.effectFile = path
}

// As gives f the type key t in place of Type, so a second fake can sit
// beside the first. Call it before Adapter.
func (f *Factory) As(t model.TypeKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typ = t
}

// Effect is one provider edit, delete or reaction the fake applied.
type Effect struct {
	// Kind is "edit", "delete", "react", "receipt", "member" or "state".
	Kind string
	// Target is the provider message acted on.
	Target model.ExternalRef
	// Text is an edit's replacement text; Emoji and Remove a reaction.
	Text  string
	Emoji string
	// Remove is a reaction removed, or a participant removed.
	Remove bool
	// Receipt is a receipt's kind (read or delivered); Participant the
	// identity a membership change names, as the adapter reports it.
	Receipt     string
	Participant model.ExternalRef
	// State is a provider conversation state change, for Kind "state".
	State StateChange
}

// Effects receives each edit, delete or reaction as it is applied.
func (f *Factory) Effects() <-chan Effect { return f.effectLog }

// IssueActs are the transitions DeclareWorkItemActs declares: those of
// an issue tracker with open and closed states only, as GitHub's.
func IssueActs() []adapter.WorkItemTransition {
	return []adapter.WorkItemTransition{
		{To: model.WorkItemClosed, From: []string{model.WorkItemOpen}, Reasons: []string{"completed", "not_planned"}},
		{To: model.WorkItemOpen, From: []string{model.WorkItemClosed}, Reasons: []string{"reopened"}},
	}
}

// DeclareWorkItemActs makes f declare work_item_act with acts, or with
// IssueActs when none is given, and accept the act: each request is
// recorded (see Acts) and answered delivered with no work item state,
// as a provider whose success carries none does, so the work item
// changes only when the provider's own report arrives through ingress.
// Call it before the engine starts.
func (f *Factory) DeclareWorkItemActs(acts ...adapter.WorkItemTransition) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acts = true
	if len(acts) == 0 {
		acts = IssueActs()
	}
	f.actSet = acts
}

func (f *Factory) workItemActs() []adapter.WorkItemTransition {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.acts {
		return nil
	}
	return slices.Clone(f.actSet)
}

// Act is one work item act the fake applied.
type Act struct {
	// Target is the provider work item acted on.
	Target model.ExternalRef
	// Status and Reason are the requested normalized status and the
	// status reason, empty for the adapter's default.
	Status string
	Reason string
}

// Acts receives each work item act as it is applied.
func (f *Factory) Acts() <-chan Act { return f.actLog }

// EnableReconcile makes f declare reconcile, as an adapter that can
// look a sent message up at its provider does. Until ReconcileAs says
// otherwise, a reconciliation answers that the provider is unavailable,
// which leaves the delivery unresolved. Call it before the engine
// starts.
func (f *Factory) EnableReconcile() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconcile = true
	f.reconErr = model.NewError(model.Unavailable, "provider lookup unavailable")
}

// ReconcileFinds makes a reconciliation find the message at the
// provider: it answers that the attempt was applied, under a provider
// reference of its own. Call it after EnableReconcile.
func (f *Factory) ReconcileFinds() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconMode = "finds"
}

// ReconcileMisses makes a reconciliation find no record of the message
// at the provider, which uma takes as "not applied" only past the settle
// window. Call it after EnableReconcile.
func (f *Factory) ReconcileMisses() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconMode = "misses"
}

// ReconcileSettle sets the settle window f declares: how long after an
// attempt a lookup that finds nothing proves nothing. Call it before
// the engine starts.
func (f *Factory) ReconcileSettle(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settle = d
}

// ReconcileAs sets what a reconciliation answers: out, or err when it
// is not nil. An outcome in state failed says the provider holds no
// such message.
func (f *Factory) ReconcileAs(out adapter.Outcome, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconOut, f.reconErr = out, err
}

// Reconciles counts the reconciliations asked of f.
func (f *Factory) Reconciles() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reconciles
}

func (f *Factory) reconcileSettle() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settle
}

// capabilities is what f declares.
func (f *Factory) capabilities() []model.Capability {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := caps
	if f.notes {
		out = append(slices.Clone(out), model.CapabilityInternalNote)
	}
	if f.reconcile {
		out = append(slices.Clone(out), model.CapabilityReconcile)
	}
	if f.effects {
		out = append(slices.Clone(out), model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact)
	}
	if f.acts {
		out = append(slices.Clone(out), model.CapabilityWorkItemAct)
	}
	if len(f.shipped) > 0 {
		out = append(slices.Clone(out), model.CapabilityExtensionExecute)
	}
	if f.state != nil {
		out = append(slices.Clone(out), f.state.Caps...)
	}
	if f.recipients != nil {
		out = append(slices.Clone(out), model.CapabilityReplyRecipients)
	}
	if f.raw != nil {
		out = append(slices.Clone(out), model.CapabilityRawRead)
	}
	if f.boxes != nil {
		out = append(slices.Clone(out), f.mailboxCapabilities()...)
	}
	for _, c := range []struct {
		on  bool
		cap model.Capability
	}{{f.members, model.CapabilityMembership}, {f.receipts, model.CapabilityReceipt}, {f.history, model.CapabilityHistory}} {
		if c.on {
			out = append(slices.Clone(out), c.cap)
		}
	}
	return out
}

// HoldReconciles makes later lookups block until ReleaseReconciles, so a
// test can hold the lease of an outcome_unknown operation a worker is
// reconciling.
func (f *Factory) HoldReconciles() {
	f.mu.Lock()
	f.reconGate = make(chan struct{})
	f.mu.Unlock()
}

// ReleaseReconciles lets held lookups answer, and later ones too.
func (f *Factory) ReleaseReconciles() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reconGate != nil {
		close(f.reconGate)
		f.reconGate = nil
	}
}

// Reconciling receives a signal as each lookup starts.
func (f *Factory) Reconciling() <-chan struct{} { return f.reconciling }

// Sending receives each send request as it starts.
func (f *Factory) Sending() <-chan adapter.SendRequest { return f.sending }

// Interrupted counts sends whose context ended before they completed.
func (f *Factory) Interrupted() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.interrupted
}

// Receiving reports how many receivers run for channel id now.
func (f *Factory) Receiving(id model.ID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.receiving[id]
}

// Runs counts the receivers ever started for channel id: a receiver
// restarted counts again.
func (f *Factory) Runs(id model.ID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[id]
}

// Adapter registers f with heo: its account is Config.Account; after
// ServeAfter, it reports serving.
func (f *Factory) Adapter() adapters.Adapter {
	f.mu.Lock()
	serving, shipped, reports, staggered := f.serving, slices.Clone(f.shipped), f.reports, f.staggered
	f.mu.Unlock()
	return adapters.Adapter{Factory: f, Account: Account, Serving: serving, ReportsLink: reports, Staggered: staggered, Extensions: shipped}
}

// Account reads the provider account from a fake channel's
// configuration.
func Account(ch model.Channel) (string, error) {
	var c Config
	if err := json.Unmarshal(ch.Configuration.Data, &c); err != nil {
		return "", errors.New("configuration does not parse")
	}
	if c.Account == "" {
		return "", errors.New("configuration names no account")
	}
	return c.Account, nil
}

var caps = []model.Capability{
	model.CapabilityReceive, model.CapabilityReceiveHTTP,
	model.CapabilitySend, model.CapabilityStartConversation,
	model.CapabilityProbePermissions,
}

// Describe declares every capability the fake implements.
func (f *Factory) Describe() adapter.Descriptor {
	f.mu.Lock()
	typ := f.typ
	f.mu.Unlock()
	schema := Schema
	if typ == "" {
		typ = Type
	} else {
		// uma keeps one adapter per configuration schema name.
		schema = string(typ) + ".config"
	}
	return adapter.Descriptor{
		Type: typ, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: schema, Version: 1},
		Capabilities:        f.capabilities(),
		Events:              f.events(),
		ReconcileSettle:     f.reconcileSettle(),
		IdentityNamespaces:  []string{Namespace},
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
		WorkItemActs:        f.workItemActs(),
	}
}

// Validate accepts a configuration naming an account.
func (f *Factory) Validate(e model.Extension) []model.ValidationIssue {
	if _, err := Account(model.Channel{Configuration: e}); err != nil {
		return []model.ValidationIssue{{Field: "account", Code: "required", Message: err.Error()}}
	}
	return nil
}

// Open opens a channel; a Broken one fails permanently. The instance
// implements the lookup only when f declares reconcile, as an adapter's
// does: one that declares none cannot reconcile (uma words that
// refusal by the missing implementation, not by the declaration). It
// reports its receiver's link when the factory or the channel says so.
func (f *Factory) Open(ctx context.Context, ch model.Channel, _ adapter.Dependencies) (adapter.Instance, error) {
	f.mu.Lock()
	slow := f.openDelay
	f.mu.Unlock()
	if slow > 0 {
		select {
		case <-time.After(slow):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	inst, err := f.open(ch)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	declares := f.reconcile
	reports := f.reports || inst.cfg.Link != ""
	f.mu.Unlock()
	switch {
	case declares && reports:
		return reportingReconciler{reporting{inst}}, nil
	case declares:
		return reconcilerInstance{inst}, nil
	case reports:
		return reporting{inst}, nil
	}
	return inst, nil
}

func (f *Factory) open(ch model.Channel) (*instance, error) {
	var c Config
	_ = json.Unmarshal(ch.Configuration.Data, &c)
	if c.Broken {
		return nil, model.NewError(model.Permanent, "fake channel is broken")
	}
	return &instance{f: f, ch: ch, cfg: c}, nil
}

// reconcilerInstance is the instance of a factory that declared
// reconcile: the one that implements adapter.Reconciler.
type reconcilerInstance struct{ *instance }

// reportingReconciler is the instance of a factory that declared
// reconcile and whose receiver reports its link.
type reportingReconciler struct{ reporting }

// Reconcile answers as reconcilerInstance does.
func (i reportingReconciler) Reconcile(ctx context.Context, r adapter.ReconcileRequest) (adapter.Outcome, error) {
	return i.reconcile(ctx, r)
}

// Reconcile answers as ReconcileAs set, and counts the question. The
// engine asks only of a channel whose factory declared reconcile.
func (i reconcilerInstance) Reconcile(ctx context.Context, r adapter.ReconcileRequest) (adapter.Outcome, error) {
	return i.reconcile(ctx, r)
}

type instance struct {
	f   *Factory
	ch  model.Channel
	cfg Config
	// link is the report function a reporting instance was handed.
	link func(bool)
}

// Capabilities reports every capability available, but receive_http
// for a Config.NoWebhook channel.
func (i *instance) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	i.f.mu.Lock()
	stall := i.f.stall
	i.f.mu.Unlock()
	if stall {
		select {
		case i.f.stalled <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	var c Config
	_ = json.Unmarshal(i.ch.Configuration.Data, &c)
	caps := i.f.capabilities()
	out := make([]model.CapabilityStatus, 0, len(caps))
	for _, name := range caps {
		st := model.CapabilityStatus{Name: name, Available: true}
		if name == model.CapabilityReceiveHTTP && c.NoWebhook {
			st = model.CapabilityStatus{Name: name, Reason: model.Unsupported}
		}
		if l, ok := i.f.stateLimits(name); ok {
			st.Limits = l
		}
		if slices.Contains(c.Disabled, name) {
			st = model.CapabilityStatus{Name: name, Reason: model.Disabled}
		}
		if slices.Contains(c.Withdrawn, name) {
			st = model.CapabilityStatus{Name: name, Reason: model.Unauthorized}
		}
		out = append(out, st)
	}
	return out, nil
}

// Probe is the provider permission probe: every capability is
// permitted except the channel's Config.Denied.
func (i *instance) Probe(context.Context) ([]model.CapabilityStatus, error) {
	var c Config
	_ = json.Unmarshal(i.ch.Configuration.Data, &c)
	caps := i.f.capabilities()
	out := make([]model.CapabilityStatus, 0, len(caps))
	for _, name := range caps {
		if slices.Contains(c.Disabled, name) || slices.Contains(c.Withdrawn, name) ||
			(name == model.CapabilityReceiveHTTP && c.NoWebhook) {
			// The probe speaks of permissions it found; it leaves what
			// the instance itself reported as it is.
			continue
		}
		st := model.CapabilityStatus{Name: name, Available: true}
		// uma merges the probe over the instance's own report: a status
		// without the state limits would drop the ones Capabilities gave.
		if l, ok := i.f.stateLimits(name); ok {
			st.Limits = l
		}
		if slices.Contains(c.Denied, name) {
			st = model.CapabilityStatus{Name: name, Reason: model.Unauthorized}
		}
		out = append(out, st)
	}
	return out, nil
}

func (i *instance) Close(context.Context) error { return nil }

// Run receives until ctx ends, or runs the script ScriptReceive installed;
// after ServeAfter, it reports serving once the delay has passed.
func (i *instance) Run(ctx context.Context, sink adapter.Sink) error {
	i.f.mu.Lock()
	i.f.receiving[i.ch.Ref.ID]++
	i.f.runs[i.ch.Ref.ID]++
	n := i.f.runs[i.ch.Ref.ID]
	serving, after, script := i.f.serving, i.f.serveAfter, i.f.script
	i.f.mu.Unlock()
	defer func() {
		i.f.mu.Lock()
		i.f.receiving[i.ch.Ref.ID]--
		i.f.mu.Unlock()
	}()
	if serving != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(after):
		}
		i.f.mu.Lock()
		i.f.served[i.ch.Ref.ID] = true
		i.f.mu.Unlock()
		serving.Report(i.ch.Ref, nil)
	}
	if script == nil {
		script = i.configured
	}
	return i.runScript(ctx, sink, n, script)
}

// Handler answers every webhook request 204 without accepting anything,
// naming the channel revision it was opened at (RevisionHeader), but
// StreamRoute, which it holds open until the request ends, and
// InjectRoute, whose Inject it hands to the sink.
func (i *instance) Handler(sink adapter.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(RevisionHeader, string(i.ch.Revision))
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, InjectRoute) {
			i.inject(w, r, sink)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, HistoryRoute) {
			i.hold(w, r)
			return
		}
		if r.URL.Path != StreamRoute {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("open\n"))
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	})
}

// Send records the request, waits for the gate and any DelaySends
// delay, then reports the message accepted, unless ctx ends first or
// FailSends set a failure.
func (i *instance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	i.f.mu.Lock()
	gate, delay, log, failure, routes := i.f.gate, i.f.sendDelay, i.f.sendLog, i.f.sendErr, i.f.routeLog
	effects := i.f.effectFile
	i.f.mu.Unlock()
	logSend(log, "start", r.Operation.ID)
	if routes != "" {
		parent := r.Route.ParentID
		if parent == "" {
			parent = "-"
		}
		appendLine(routes, fmt.Sprintf("%s %s %s %s", r.Operation.ID, r.Route.Kind, parent, r.Route.ID))
	}
	select {
	case i.f.sending <- r:
	default:
	}
	delayed := make(chan struct{})
	if delay > 0 {
		t := time.AfterFunc(delay, func() { close(delayed) })
		defer t.Stop()
	} else {
		close(delayed)
	}
	for _, wait := range []<-chan struct{}{gate, delayed} {
		select {
		case <-wait:
		case <-ctx.Done():
			i.f.mu.Lock()
			i.f.interrupted++
			i.f.mu.Unlock()
			logSend(log, "interrupted", r.Operation.ID)
			return adapter.Outcome{}, ctx.Err()
		}
	}
	if failure != nil {
		logSend(log, "failed", r.Operation.ID)
		return adapter.Outcome{}, failure
	}
	logSend(log, "done", r.Operation.ID)
	if rec := r.Recipients; rec != nil {
		appendLine(effects, fmt.Sprintf("reply %s reply_all=%t cc=%s bcc=%s", r.Route.ID, rec.ReplyAll, strings.Join(rec.Cc, ","), strings.Join(rec.Bcc, ",")))
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{
		model.ProviderMessageRef(r.Route.Channel, "fake-"+r.Route.ID),
	}}, nil
}

func (i *instance) reconcile(ctx context.Context, r adapter.ReconcileRequest) (adapter.Outcome, error) {
	i.f.mu.Lock()
	i.f.reconciles++
	gate, mode, out, err := i.f.reconGate, i.f.reconMode, i.f.reconOut, i.f.reconErr
	i.f.mu.Unlock()
	select {
	case i.f.reconciling <- struct{}{}:
	default:
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return adapter.Outcome{}, ctx.Err()
		}
	}
	switch mode {
	case "finds":
		return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{
			model.ProviderMessageRef(r.Route.Channel, "fake-found-"+r.Route.ID),
		}}, nil
	case "misses":
		return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.NotFound, "no such message")}, nil
	}
	return out, err
}

// SendNote delivers an internal note as a private provider note, after
// DeliverNotes.
func (i *instance) SendNote(_ context.Context, r adapter.NoteRequest) (adapter.Outcome, error) {
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{
		model.ProviderMessageRef(r.Route.Channel, "fake-note-"+r.Route.ID),
	}}, nil
}

// effect records an applied effect and reports the provider accepted it.
func (i *instance) effect(e Effect, target *model.ExternalRef) (adapter.Outcome, error) {
	if target != nil {
		e.Target = *target
	}
	i.f.mu.Lock()
	path := i.f.effectFile
	i.f.mu.Unlock()
	appendLine(path, effectLine(e))
	select {
	case i.f.effectLog <- e:
	default:
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// Edit applies an edit, after DeclareEffects.
func (i *instance) Edit(_ context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	text := ""
	for _, p := range r.Content.Parts {
		if p.Kind == model.PartText {
			text += p.Text
		}
	}
	return i.effect(Effect{Kind: "edit", Text: text}, r.Target)
}

// Delete applies a deletion, after DeclareEffects.
func (i *instance) Delete(_ context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	return i.effect(Effect{Kind: "delete"}, r.Target)
}

// React applies a reaction, or removes it, after DeclareEffects.
func (i *instance) React(_ context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	return i.effect(Effect{Kind: "react", Emoji: r.Emoji, Remove: r.Remove}, r.Target)
}

// ActWorkItem applies a work item act, after DeclareWorkItemActs: it
// records the request, and answers it delivered with no work item
// state. With LogEffects set it appends "act <issue> <status> <reason>"
// to the file, the issue being the provider's "parent#number".
func (i *instance) ActWorkItem(_ context.Context, r adapter.WorkItemActRequest) (adapter.WorkItemActOutcome, error) {
	a := Act{Target: r.Route, Status: r.Status, Reason: r.Reason}
	i.f.mu.Lock()
	path := i.f.effectFile
	i.f.mu.Unlock()
	if path != "" {
		line := "act " + r.Route.ParentID + "#" + r.Route.ID + " " + r.Status
		if r.Reason != "" {
			line += " " + r.Reason
		}
		if fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil { //nolint:gosec // test-chosen path
			_, _ = fmt.Fprintln(fh, line)
			_ = fh.Close()
		}
	}
	select {
	case i.f.actLog <- a:
	default:
	}
	return adapter.WorkItemActOutcome{Outcome: adapter.Outcome{State: model.Delivered}}, nil
}
