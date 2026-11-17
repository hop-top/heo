package approvechat

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// Reader reads a held operation as actor: uma authorizes the read for
// it, so an operation the actor may not read is model.NotFound.
type Reader interface {
	Get(ctx context.Context, actor model.ActorID, id string) (desk.HeldOperation, error)
}

// Decider decides a held operation as actor, through
// desk.Service.DecideHeld.
type Decider interface {
	Decide(ctx context.Context, actor model.ActorID, r desk.DecideRequest) (desk.HeldDecided, error)
}

// Poster speaks in an approval conversation as the daemon's runtime.
type Poster interface {
	// Post sends text to c, as a reply to message replyTo when it is
	// not empty, and returns the sent message's ID.
	Post(ctx context.Context, c Conversation, replyTo, text string) (string, error)
	// Edit replaces the text of a message the Poster sent.
	Edit(ctx context.Context, c Conversation, message, text string) error
}

// Logger receives what the handler ignores or refuses.
type Logger interface {
	Info(msg any, keyvals ...any)
	Error(msg any, keyvals ...any)
}

// Handler is the notifier and the inbound handler of one daemon.
type Handler struct {
	cfg    Config
	read   Reader
	decide Decider
	post   Poster
	log    Logger

	// inbound serializes decisions: one event is decided once even
	// when a provider delivers it twice at the same moment.
	inbound sync.Mutex
	mu      sync.Mutex
	anns    map[string]string    // conversation key + "\x00" + message -> operation
	byOp    map[string][]annRef  // operation -> announcements
	closed  map[string]closedRef // like anns, for announcements whose operation closed
	seen    map[string]bool
	ring    []string
	now     func() time.Time

	// store keeps the announcements and the handled events across a
	// restart; nil keeps them in memory only. persistMu orders saves.
	store     Store
	persistMu sync.Mutex
}

type annRef struct {
	conv    Conversation
	message string
}

// closedRef is an announcement whose operation was decided or expired:
// kept for ClosedRetention so a late reply is recognised and ignored.
type closedRef struct {
	conv    Conversation
	message string
	op      string
	outcome string // the operation's decision: approved, rejected, expired; closed when unknown
	at      time.Time
}

// ClosedRetention is how long an announcement is remembered after its
// operation closed.
const ClosedRetention = 7 * 24 * time.Hour

// seenMax bounds how many reply and reaction IDs are remembered.
const seenMax = 4096

// New returns a Handler.
func New(cfg Config, r Reader, d Decider, p Poster, l Logger) *Handler {
	return &Handler{cfg: cfg, read: r, decide: d, post: p, log: l,
		anns: map[string]string{}, byOp: map[string][]annRef{}, closed: map[string]closedRef{},
		seen: map[string]bool{}, now: time.Now}
}

// WithClock has h read the time from now, which dates the closed
// announcements. It returns h.
func (h *Handler) WithClock(now func() time.Time) *Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = now
	return h
}

// closedOf returns the closed announcement at key, none once it is past
// retention. The caller holds h.mu.
func (h *Handler) closedOf(key string) (closedRef, bool) {
	c, ok := h.closed[key]
	if !ok || h.now().Sub(c.at) >= ClosedRetention {
		return closedRef{}, false
	}
	return c, true
}

// pruneClosed drops the closed announcements past retention. The caller
// holds h.mu.
func (h *Handler) pruneClosed() {
	for k, c := range h.closed {
		if h.now().Sub(c.at) >= ClosedRetention {
			delete(h.closed, k)
		}
	}
}

func (h *Handler) conv(inbox, conversation string) (Conversation, bool) {
	for _, c := range h.cfg.Conversations {
		if c.Inbox == inbox && c.Conversation == conversation {
			return c, true
		}
	}
	return Conversation{}, false
}

// OnHeld announces held operation id (an operation.held event) to every
// approval conversation. Called twice for one operation it announces
// once.
func (h *Handler) OnHeld(ctx context.Context, id string) {
	if !h.cfg.Enabled {
		return
	}
	for _, c := range h.cfg.Conversations {
		h.announce(ctx, c, id)
	}
}

func (h *Handler) announce(ctx context.Context, c Conversation, id string) {
	h.mu.Lock()
	for _, a := range h.byOp[id] {
		if a.conv.Key() == c.Key() {
			h.mu.Unlock()
			return
		}
	}
	h.mu.Unlock()
	// The preview is for a conversation whose every mapped member may
	// read the operation; one that may not makes it an id alone.
	var op desk.HeldOperation
	var readers int
	for _, m := range c.Members {
		v, err := h.read.Get(ctx, m.Actor, id)
		if err != nil {
			continue
		}
		op, readers = v, readers+1
	}
	if readers == 0 {
		h.log.Info("approval chat: no member may read the operation; not announced", "operation", id, "conversation", c.Key())
		return
	}
	text := idOnlyText(id)
	if readers == len(c.Members) {
		text = announcement(op)
	} else {
		h.log.Info("approval chat: preview withheld, not every member may read it", "operation", id, "conversation", c.Key())
	}
	msg, err := h.post.Post(ctx, c, "", text)
	if err != nil {
		h.log.Error("approval chat: announce failed", "operation", id, "conversation", c.Key(), "error", err)
		return
	}
	h.mu.Lock()
	h.anns[c.Key()+"\x00"+msg] = id
	h.byOp[id] = append(h.byOp[id], annRef{c, msg})
	h.mu.Unlock()
	h.persist()
}

// OnClosed closes the announcements of operation id (an operation.decided
// or operation.expired event): the text becomes the outcome, with no
// preview.
func (h *Handler) OnClosed(ctx context.Context, id string) {
	h.mu.Lock()
	refs := h.byOp[id]
	h.mu.Unlock()
	if len(refs) == 0 {
		return
	}
	text, result := fmt.Sprintf("Held operation %s: closed.", id), "closed"
read:
	for _, a := range refs {
		for _, m := range a.conv.Members {
			if v, err := h.read.Get(ctx, m.Actor, id); err == nil {
				text = outcome(v)
				if v.Decision != "" && v.Decision != string(model.HeldEdited) {
					result = v.Decision
				}
				break read
			}
		}
	}
	h.mu.Lock()
	at := h.now()
	delete(h.byOp, id)
	for _, a := range refs {
		k := a.conv.Key() + "\x00" + a.message
		delete(h.anns, k)
		h.closed[k] = closedRef{conv: a.conv, message: a.message, op: id, outcome: result, at: at}
	}
	h.pruneClosed()
	h.mu.Unlock()
	h.persist()
	for _, a := range refs {
		if err := h.post.Edit(ctx, a.conv, a.message, text); err != nil {
			h.log.Error("approval chat: closing announcement failed", "operation", id, "error", err)
		}
	}
}

// Inbound is a reply or a reaction in an approval conversation, as the
// channel reported it.
type Inbound struct {
	// Event is the provider's own ID of this reply or reaction: the
	// same one twice is decided once.
	Event               string
	Inbox, Conversation string
	// Identity is the record ID of the sender's uma identity.
	Identity string
	// Text is a reply's text; Emoji a reaction's emoji, set instead.
	Text, Emoji string
	// To is the message the reply answers or the reaction is on. A
	// reply carries it only when the channel reported what it answers
	// (uma's reply parent): a thread reply, a quote reply.
	To string
	// Thread is set when To is a thread's root, not a message the sender
	// quoted: a channel with threads and no quote reply (Slack) reports
	// every reply in a thread that way, so a reply to any message of the
	// announcement's thread arrives as a reply to the announcement.
	Thread bool
	// Own is set for a message heo or another system actor wrote: the
	// announcement's own sender never decides, whoever it maps to.
	Own bool
}

// OnInbound handles a reply or reaction.
func (h *Handler) OnInbound(ctx context.Context, in Inbound) {
	if !h.cfg.Enabled {
		return
	}
	conv, ok := h.conv(in.Inbox, in.Conversation)
	if !ok {
		return
	}
	if in.Event == "" {
		h.log.Info("approval chat: event without an ID ignored", "conversation", conv.Key())
		return
	}
	if in.Own {
		h.log.Info("approval chat: own or system message ignored", "conversation", conv.Key(), "event", in.Event)
		return
	}
	actor, ok := conv.actorOf(in.Identity)
	if !ok {
		h.log.Info("approval chat: unmapped user ignored", "conversation", conv.Key(), "identity", in.Identity, "event", in.Event)
		return
	}
	// A thread reply is not an answer to the announcement unless the
	// conversation says so: only the explicit form, or a reaction,
	// decides.
	to := in.To
	if in.Thread && !conv.SlackThreadReplies {
		if _, bare := ParseReply(in.Text, true); bare && in.Emoji == "" {
			if _, explicit := ParseReply(in.Text, false); !explicit {
				h.log.Info("approval chat: thread reply does not decide; say `approve #<operation>` or react",
					"conversation", conv.Key(), "event", in.Event)
			}
		}
		to = ""
	}
	var cmd Command
	if in.Emoji != "" {
		cmd, ok = ParseReaction(in.Emoji)
	} else {
		h.mu.Lock()
		annOp, onAnn := h.anns[conv.Key()+"\x00"+to]
		_, onClosed := h.closedOf(conv.Key() + "\x00" + to)
		h.mu.Unlock()
		cmd, ok = ParseReply(in.Text, onAnn || onClosed)
		// A reply to an announcement decides that announcement's
		// operation and no other: one naming another is not a decision.
		if ok && onAnn && cmd.Operation != "" && cmd.Operation != annOp {
			h.log.Info("approval chat: reply names another operation than its announcement; ignored",
				"conversation", conv.Key(), "event", in.Event, "announced", annOp, "named", cmd.Operation)
			return
		}
		if ok && onClosed {
			h.logClosed(conv, to, in)
			return
		}
	}
	if !ok {
		return
	}
	if in.Emoji != "" && h.logClosed(conv, in.To, in) {
		return
	}
	h.inbound.Lock()
	defer h.inbound.Unlock()
	h.mu.Lock()
	if cmd.Operation == "" {
		cmd.Operation = h.anns[conv.Key()+"\x00"+to]
	}
	if cmd.Operation == "" || h.seen[in.Event] {
		dup := h.seen[in.Event]
		h.mu.Unlock()
		if dup {
			h.log.Info("approval chat: repeated event ignored", "event", in.Event)
		}
		return
	}
	h.mu.Unlock()

	reply, final := h.run(ctx, conv, actor, cmd, in)
	if final {
		h.remember(in.Event)
		h.persist()
	}
	if reply != "" {
		if _, err := h.post.Post(ctx, conv, in.To, reply); err != nil {
			h.log.Error("approval chat: answer failed", "event", in.Event, "error", err)
		}
	}
}

// logClosed logs that the message at to is an announcement whose
// operation closed, and reports whether it is. Nothing is posted and
// nothing decided: a late reply or reaction is only noted.
func (h *Handler) logClosed(conv Conversation, to string, in Inbound) bool {
	h.mu.Lock()
	c, ok := h.closedOf(conv.Key() + "\x00" + to)
	h.mu.Unlock()
	if !ok {
		return false
	}
	msg := "approval chat: already decided; reply ignored"
	if c.outcome == string(model.HeldExpired) {
		msg = "approval chat: expired; reply ignored"
	}
	h.log.Info(msg, "conversation", conv.Key(), "operation", c.op, "outcome", c.outcome, "event", in.Event)
	return true
}

// run decides and returns the answer to post and whether the outcome is
// final (the event is not to be handled again).
func (h *Handler) run(ctx context.Context, conv Conversation, actor model.ActorID, cmd Command, in Inbound) (string, bool) {
	if actor.IsPlugin() {
		h.log.Info("approval chat: plugin actor refused", "conversation", conv.Key(), "event", in.Event)
		return "A plugin actor may not decide a held operation.", true
	}
	op, err := h.read.Get(ctx, actor, cmd.Operation)
	if err != nil {
		return h.refusal(err, cmd.Operation, in)
	}
	if op.Status != string(model.Held) {
		return closedAnswer(op), true
	}
	d := desk.DecideApprove
	if cmd.Decision == "reject" {
		d = desk.DecideReject
	}
	res, err := h.decide.Decide(ctx, actor, desk.DecideRequest{
		ID: cmd.Operation, Decision: d, Note: cmd.Note, Correlation: "chat:" + in.Event})
	if err != nil {
		return h.refusal(err, cmd.Operation, in)
	}
	if res.Replayed {
		h.log.Info("approval chat: already decided", "operation", cmd.Operation, "event", in.Event)
		return "", true
	}
	return fmt.Sprintf("Held operation %s: %s by %s.", cmd.Operation, pastTense(cmd.Decision), actor), true
}

func (h *Handler) refusal(err error, id string, in Inbound) (string, bool) {
	switch {
	case desk.IsStale(err):
		return "The target changed since the operation was held. Read it with `heo operation held show " + id + "`.", true
	case model.IsCode(err, model.Unauthorized):
		h.log.Info("approval chat: not permitted", "operation", id, "event", in.Event)
		return "You may not decide this operation.", true
	case model.IsCode(err, model.NotFound):
		return "Operation " + id + " not found, or you may not read it.", true
	case model.IsCode(err, model.Conflict):
		return "Operation " + id + " can no longer be decided.", true
	}
	h.log.Error("approval chat: decision failed", "operation", id, "event", in.Event, "error", err)
	// Not final: the provider may redeliver, and uma's correlation
	// keeps a second attempt from deciding twice.
	return "", false
}

func (h *Handler) remember(event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen[event] {
		return
	}
	h.seen[event] = true
	h.ring = append(h.ring, event)
	if len(h.ring) > seenMax {
		delete(h.seen, h.ring[0])
		h.ring = h.ring[1:]
	}
}

func pastTense(decision string) string {
	if decision == "reject" {
		return "rejected"
	}
	return "approved"
}

func closedAnswer(op desk.HeldOperation) string {
	switch {
	case op.Decision == string(model.HeldExpired):
		return "Held operation " + op.ID + " expired; it can no longer be decided."
	case op.Decision != "" && op.Decision != string(model.HeldEdited):
		return "Held operation " + op.ID + " is already " + op.Decision + "."
	}
	return "Held operation " + op.ID + " is " + op.Status + "; it can no longer be decided."
}

func outcome(op desk.HeldOperation) string {
	switch op.Decision {
	case string(model.HeldExpired):
		return "Held operation " + op.ID + ": expired."
	case "":
		return "Held operation " + op.ID + ": " + op.Status + "."
	}
	by := ""
	if op.DecidedBy != "" {
		by = " by " + op.DecidedBy
	}
	return "Held operation " + op.ID + ": " + op.Decision + by + "."
}

// previewMax bounds the preview in an announcement.
const previewMax = 1500

func idOnlyText(id string) string {
	return "Held operation " + id + " needs a decision. Details withheld: read it with `heo operation held show " + id + "`."
}

func announcement(op desk.HeldOperation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Held operation %s needs a decision.\nkind: %s", op.ID, op.Kind)
	if op.Target != "" {
		fmt.Fprintf(&b, "  target: %s", op.Target)
	}
	fmt.Fprintf(&b, "\nproposer: %s", op.Proposer)
	if op.Reason != "" {
		fmt.Fprintf(&b, "\nreason: %s", op.Reason)
	}
	if op.ExpiresAt != nil {
		fmt.Fprintf(&b, "\nexpires: %s", op.ExpiresAt.UTC().Format("2006-01-02 15:04 MST"))
	}
	if len(op.Preview) > 0 {
		p := []rune(string(op.Preview))
		if len(p) > previewMax {
			p = append(p[:previewMax], '…')
		}
		fmt.Fprintf(&b, "\npreview: %s", string(p))
	}
	fmt.Fprintf(&b, "\nReply `approve` or `reject <note>` to this message, say `approve #%s` or `reject #%s <note>`, or react 👍 / 👎.", op.ID, op.ID)
	return b.String()
}
