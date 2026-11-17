package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/heo/internal/approvechat"
	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// chatCorrelation tags the approval chat's calls in uma.
const chatCorrelation model.CorrelationID = "heo-approval-chat"

// Subscription pacing, as the bridge's.
const (
	chatPoll     = 5 * time.Second
	chatBatch    = 100
	chatRetryMin = time.Second
	chatRetryMax = time.Minute
	chatGrace    = 10 * time.Second
)

// chatKinds are the events the service reads: the held operation's
// lifecycle, inbound messages and the reactions on them.
var chatKinds = []string{
	model.EventOperationHeld, model.EventOperationDecided, model.EventOperationExpired,
	model.EventMessageReceived, model.EventMessageUpdated,
}

// chatRunner is one open of the approval chat: the handler, and the
// subscription that feeds it.
type chatRunner struct {
	r      *running
	cfg    approvechat.Config
	client uma.Client
	h      *approvechat.Handler
	report func(failure string)
	seq    atomic.Uint64

	mu       sync.Mutex
	inboxes  map[string]string // configured inbox (ID or key) -> inbox record ID
	threaded map[string]bool   // configured inbox -> its channel reports thread replies only

	// sub and unacked belong to the loop alone.
	sub     model.SubscriptionRef
	unacked string
}

// newChatRunner builds the runner over the open runtime r.
func newChatRunner(r *running, cfg approvechat.Config, report func(string)) (*chatRunner, error) {
	client, err := r.engine.For(model.Principal{Tenant: r.deps.Tenant, Actor: authz.ApprovalChatActor})
	if err != nil {
		return nil, err
	}
	c := &chatRunner{r: r, cfg: cfg, client: client, report: report, inboxes: map[string]string{}, threaded: map[string]bool{}}
	c.h = approvechat.New(cfg, chatReader{c}, chatDecider{c}, chatPoster{c}, chatLog{r})
	if dir := chatStateDir(r.deps); dir != "" {
		c.h.WithStore(approvechat.NewFileStore(dir))
	}
	return c, nil
}

// chatStateDir is where the announcements' references are kept so a
// restart still edits them: beside the plugins' directories, as the hook
// dead letters are. None, when the daemon has no state directory,
// keeps them in memory.
func chatStateDir(d Deps) string {
	if d.PluginsDir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(d.PluginsDir), "approval-chat")
}

// chatLog adapts the runtime's logger to the handler's.
type chatLog struct{ r *running }

func (l chatLog) Info(msg any, kv ...any)  { l.r.logInfo(fmt.Sprint(msg), kv...) }
func (l chatLog) Error(msg any, kv ...any) { l.r.logError(fmt.Sprint(msg), kv...) }

// deskFor is the desk of actor over the open runtime: every call runs
// as actor, so uma and heo's policy authorize it for that actor and no
// other.
func (c *chatRunner) deskFor(actor model.ActorID) (desk.Service, error) {
	p := model.Principal{Tenant: c.r.deps.Tenant, Actor: actor}
	client, err := c.r.engine.For(p)
	if err != nil {
		return desk.Service{}, err
	}
	return desk.Service{
		Client: client, Tenant: c.r.deps.Tenant, Actor: actor,
		Sees: func(au model.Audience) bool {
			d, err := c.r.deps.Authorizer.Authorize(context.Background(), policy.Authorization{
				Principal: p, Action: model.ActionMessageRead, Audience: au})
			return err == nil && d.Allow
		},
	}, nil
}

// chatReader reads a held operation as the mapped actor.
type chatReader struct{ c *chatRunner }

func (p chatReader) Get(ctx context.Context, actor model.ActorID, id string) (desk.HeldOperation, error) {
	s, err := p.c.deskFor(actor)
	if err != nil {
		return desk.HeldOperation{}, err
	}
	return s.GetHeld(ctx, id)
}

// chatDecider decides as the mapped actor, through the desk's
// DecideHeld, which a plugin actor never passes.
type chatDecider struct{ c *chatRunner }

func (p chatDecider) Decide(ctx context.Context, actor model.ActorID, r desk.DecideRequest) (desk.HeldDecided, error) {
	s, err := p.c.deskFor(actor)
	if err != nil {
		return desk.HeldDecided{}, err
	}
	return s.DecideHeld(ctx, r)
}

// chatPoster speaks as the service's own actor.
type chatPoster struct{ c *chatRunner }

// key is a fresh operation ID: every post is its own operation.
func (c *chatRunner) key(what string) model.OperationID {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return model.OperationID(fmt.Sprintf("approval-chat-%s-%d-%s", what, c.seq.Add(1), hex.EncodeToString(b[:])))
}

func (c *chatRunner) convRef(ctx context.Context, cv approvechat.Conversation) (model.ConversationRef, error) {
	id, err := c.inboxID(ctx, cv.Inbox)
	if err != nil {
		return model.ConversationRef{}, err
	}
	return model.ConversationRef{Inbox: model.InboxRef{Tenant: c.r.deps.Tenant, ID: model.ID(id)}, ID: model.ID(cv.Conversation)}, nil
}

func (p chatPoster) Post(ctx context.Context, cv approvechat.Conversation, replyTo, text string) (string, error) {
	c := p.c
	ref, err := c.convRef(ctx, cv)
	if err != nil {
		return "", err
	}
	req := model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: chatCorrelation}, Operation: c.key("post")},
		Conversation: ref, Content: desk.TextContent(text),
	}
	if replyTo != "" {
		req.InReplyTo = &model.MessageRef{Conversation: ref, ID: model.ID(replyTo)}
	}
	acc, err := c.client.Messages().Reply(ctx, req)
	if err != nil && req.InReplyTo != nil {
		// A parent uma does not know leaves the answer unthreaded.
		req.InReplyTo, req.Operation = nil, c.key("post")
		acc, err = c.client.Messages().Reply(ctx, req)
	}
	if err != nil {
		return "", err
	}
	if acc.Operation.ResultMessage == nil {
		return "", fmt.Errorf("uma recorded no message for the reply")
	}
	return string(acc.Operation.ResultMessage.ID), nil
}

func (p chatPoster) Edit(ctx context.Context, cv approvechat.Conversation, message, text string) error {
	c := p.c
	ref, err := c.convRef(ctx, cv)
	if err != nil {
		return err
	}
	mref := model.MessageRef{Conversation: ref, ID: model.ID(message)}
	m, err := c.delivered(ctx, mref)
	if err != nil {
		return err
	}
	_, err = c.client.Messages().Edit(ctx, model.EditMessage{
		Command: model.Command{Call: model.Call{Correlation: chatCorrelation}, Operation: c.key("edit"), ExpectedRevision: m.Revision},
		Message: mref, Content: desk.TextContent(text),
	})
	return err
}

// deliveryWait bounds how long an edit waits for the provider to have
// the message it edits; deliveryPoll is how often it looks.
const (
	deliveryWait = 15 * time.Second
	deliveryPoll = 50 * time.Millisecond
)

// delivered returns the message once the provider holds it: a message
// the worker has not sent yet has no provider ID to edit, and its
// revision moves when it is sent.
func (c *chatRunner) delivered(ctx context.Context, ref model.MessageRef) (model.Message, error) {
	deadline := time.Now().Add(deliveryWait)
	for {
		m, err := c.client.Messages().Get(ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: chatCorrelation}, Ref: ref})
		if err != nil {
			return model.Message{}, err
		}
		for _, x := range m.External {
			if x.Namespace == model.ProviderMessageNamespace && x.Kind == model.RefKindMessage {
				return m, nil
			}
		}
		if time.Now().After(deadline) {
			return model.Message{}, fmt.Errorf("message %s was not delivered within %s: its announcement is not closed", ref.ID, deliveryWait)
		}
		select {
		case <-ctx.Done():
			return model.Message{}, ctx.Err()
		case <-time.After(deliveryPoll):
		}
	}
}

// inboxID resolves a configured inbox, a record ID or a key, to the
// record ID events carry.
func (c *chatRunner) inboxID(ctx context.Context, ref string) (string, error) {
	c.mu.Lock()
	id, ok := c.inboxes[ref]
	c.mu.Unlock()
	if ok {
		return id, nil
	}
	in, err := channels.Service{Client: c.client, Tenant: c.r.deps.Tenant}.FindInbox(ctx, ref)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.inboxes[ref] = string(in.Ref.ID)
	c.mu.Unlock()
	return string(in.Ref.ID), nil
}

// threadsOnly reports whether the channel of the configured inbox
// reports a reply parent only for a thread reply (Slack), never for a
// quote reply.
func (c *chatRunner) threadsOnly(ctx context.Context, inbox string) (bool, error) {
	c.mu.Lock()
	v, ok := c.threaded[inbox]
	c.mu.Unlock()
	if ok {
		return v, nil
	}
	svc := channels.Service{Client: c.client, Tenant: c.r.deps.Tenant}
	in, err := svc.FindInbox(ctx, inbox)
	if err != nil {
		return false, err
	}
	ch, err := svc.FindChannel(ctx, string(in.Channel.ID))
	if err != nil {
		return false, err
	}
	v = ch.Type == model.TypeSlackChat
	c.mu.Lock()
	c.threaded[inbox] = v
	c.mu.Unlock()
	return v, nil
}

// conversationOf returns the configured conversation m is in. A
// configured inbox that does not exist matches nothing; any other
// failure is the caller's to retry.
func (c *chatRunner) conversationOf(ctx context.Context, m model.ConversationRef) (approvechat.Conversation, bool, error) {
	for _, cv := range c.cfg.Conversations {
		if cv.Conversation != string(m.ID) {
			continue
		}
		id, err := c.inboxID(ctx, cv.Inbox)
		if model.IsCode(err, model.NotFound) {
			c.r.logError("approval chat: configured inbox not found", "inbox", cv.Inbox)
			continue
		}
		if err != nil {
			return approvechat.Conversation{}, false, err
		}
		if id == string(m.Inbox.ID) {
			return cv, true, nil
		}
	}
	return approvechat.Conversation{}, false, nil
}

// payloadOf reads the builtin payload of ev into v; one of a schema or
// version this build does not read yields false.
func payloadOf(ev model.Event, v any) bool {
	if ev.Payload.Name != "uma."+ev.Kind || ev.Payload.Version != model.PayloadVersion1 {
		return false
	}
	return json.Unmarshal(ev.Payload.Data, v) == nil
}

// handle turns one event into what the handler does with it. An error
// is a failure to retry the batch for; an event that means nothing to
// the service is not one.
func (c *chatRunner) handle(ctx context.Context, ev model.Event) error {
	switch ev.Kind {
	case model.EventOperationHeld, model.EventOperationDecided, model.EventOperationExpired:
		var p model.HeldOperationPayload
		if !payloadOf(ev, &p) {
			return nil
		}
		if ev.Kind == model.EventOperationHeld {
			c.h.OnHeld(ctx, string(p.Operation.ID))
		} else {
			c.h.OnClosed(ctx, string(p.Operation.ID))
		}
	case model.EventMessageReceived:
		var p model.MessageReceivedPayload
		if !payloadOf(ev, &p) || p.Author == nil {
			return nil
		}
		cv, ok, err := c.conversationOf(ctx, p.Message.Conversation)
		if err != nil || !ok {
			return err
		}
		// Only the text of a message in an approval conversation is read.
		m, err := c.client.Messages().Get(ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: chatCorrelation}, Ref: p.Message})
		if err != nil {
			return err
		}
		// What the channel reports the message answers, as uma resolved
		// it to a message of this conversation; none when it reported
		// none or uma does not hold the message. A reply is keyed by its
		// own message: one message decides at most once, and an edit or a
		// deletion of it is a message.updated that is never read here.
		parent := p.InReplyTo
		if parent == nil {
			parent = m.InReplyTo
		}
		var to string
		var thread bool
		if parent != nil && parent.In(p.Message.Conversation) {
			to = string(parent.ID)
			// Slack has no quote reply: a parent it reports is a thread's
			// root, whichever message of the thread was answered.
			if thread, err = c.threadsOnly(ctx, cv.Inbox); err != nil {
				return err
			}
		}
		c.h.OnInbound(ctx, approvechat.Inbound{
			Event: string(p.Message.ID), Inbox: cv.Inbox, Conversation: cv.Conversation,
			Identity: string(p.Author.ID), Text: desk.Text(m.Content), To: to, Thread: thread,
			// Written through uma, or by a system actor: heo's own words
			// echoed back are not a member's decision.
			Own: m.Origin != nil || p.AuthorKind == model.AuthorKindSystem,
		})
	case model.EventMessageUpdated:
		var p model.MessageUpdatedPayload
		// A reaction by a provider identity only: one made through uma
		// (the service's own) names none, and a count says no one.
		if !payloadOf(ev, &p) || p.Change != model.MessageChangeReactionAdded || p.Reaction == nil || p.Reaction.By == nil {
			return nil
		}
		cv, ok, err := c.conversationOf(ctx, p.Message.Conversation)
		if err != nil || !ok {
			return err
		}
		c.h.OnInbound(ctx, approvechat.Inbound{
			Event: string(ev.ID), Inbox: cv.Inbox, Conversation: cv.Conversation,
			Identity: string(p.Reaction.By.ID), Emoji: p.Reaction.Emoji, To: string(p.Message.ID),
		})
	}
	return nil
}

// open opens the service's subscription, at the tail when it is new. An
// open subscription keeps its checkpoint, which is how a restart
// resumes.
func (c *chatRunner) open(ctx context.Context) error {
	res, err := c.client.Events().Open(ctx, model.OpenSubscription{
		Command:  model.Command{Call: model.Call{Correlation: chatCorrelation}, Operation: c.key("open")},
		Consumer: ApprovalChatConsumer, Filter: model.Filter{Kinds: chatKinds},
		Start: model.StartTail, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		return err
	}
	c.sub, c.unacked = res.Subscription.Ref, ""
	c.report("")
	return nil
}

// loop reads and handles until ctx ends. A failure never ends it: it
// reports itself waiting and retries after a growing delay. A batch in
// hand when ctx ends is still handled and acknowledged, within a grace
// period.
func (c *chatRunner) loop(ctx context.Context) {
	delay := chatRetryMin
	for ctx.Err() == nil {
		err := c.step(ctx)
		if err == nil {
			delay = chatRetryMin
			continue
		}
		if ctx.Err() != nil {
			return
		}
		code := string(model.CodeOf(err))
		if code == "" {
			code = "error"
		}
		c.report(code)
		c.r.logError("approval chat: degraded; retrying", "code", code, "error", err)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		delay = min(delay*2, chatRetryMax)
	}
}

func (c *chatRunner) step(ctx context.Context) error {
	if c.sub == (model.SubscriptionRef{}) {
		if err := c.open(ctx); err != nil {
			return err
		}
	}
	if c.unacked != "" {
		return c.ack(ctx)
	}
	batch, err := c.client.Events().Poll(ctx, model.Poll{
		Call: model.Call{Correlation: chatCorrelation}, Subscription: c.sub, Limit: chatBatch, Wait: chatPoll,
	})
	if err != nil {
		switch model.CodeOf(err) {
		case model.Conflict, model.NotFound:
			c.sub = model.SubscriptionRef{}
		}
		return err
	}
	if batch.AckToken == "" {
		c.report("")
		return nil
	}
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), chatGrace)
	defer cancel()
	for _, ev := range batch.Events {
		if err := c.handle(work, ev); err != nil {
			return err
		}
	}
	c.unacked = batch.AckToken
	return c.ack(work)
}

func (c *chatRunner) ack(ctx context.Context) error {
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), chatGrace)
	defer cancel()
	sum := sha256.Sum256([]byte(c.unacked))
	_, err := c.client.Events().Ack(work, model.Acknowledge{
		Command:      model.Command{Call: model.Call{Correlation: chatCorrelation}, Operation: model.OperationID("approval-chat-ack-" + hex.EncodeToString(sum[:16]))},
		Subscription: c.sub, Token: c.unacked,
	})
	// A refused acknowledgement leaves the batch to be redelivered; the
	// handler decides an event once.
	if err != nil && model.CodeOf(err) != model.Conflict {
		return err
	}
	c.unacked = ""
	c.report("")
	return nil
}
