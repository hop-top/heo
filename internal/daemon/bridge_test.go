package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/bridge"
	"hop.top/heo/internal/daemon"
	"hop.top/kit/go/console/serve"
	"hop.top/kit/go/runtime/bus"
	"hop.top/uma"
	"hop.top/uma/model"
)

const deskActor model.ActorID = "uid:1000"

// busLog records what a subscriber of one scope received: the topic and
// the payload as JSON, the bytes a sink or a hook would see.
type busLog struct {
	mu     sync.Mutex
	topics []bus.Topic
	raw    []string
	kinds  []string
}

func (l *busLog) add(e bus.Event) {
	raw, _ := json.Marshal(e.Payload)
	kind := ""
	if p, ok := e.Payload.(bridge.Payload); ok {
		kind = p.Kind
	}
	l.mu.Lock()
	l.topics, l.raw, l.kinds = append(l.topics, e.Topic), append(l.raw, string(raw)), append(l.kinds, kind)
	l.mu.Unlock()
}

func (l *busLog) count(topic bus.Topic) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, t := range l.topics {
		if t == topic {
			n++
		}
	}
	return n
}

func (l *busLog) all() ([]bus.Topic, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]bus.Topic(nil), l.topics...), append([]string(nil), l.raw...)
}

func (l *busLog) payloads(topic bus.Topic) []bridge.Payload {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []bridge.Payload
	for i, t := range l.topics {
		if t != topic {
			continue
		}
		var p bridge.Payload
		_ = json.Unmarshal([]byte(l.raw[i]), &p)
		out = append(out, p)
	}
	return out
}

// listen subscribes a log to the hub's events of scope.
func listen(t *testing.T, h *bridge.Hub, scope bridge.Scope) *busLog {
	t.Helper()
	l := &busLog{}
	cancel := h.Subscribe(scope, "#", l.add)
	t.Cleanup(cancel)
	return l
}

// bridgeRig is a rig whose store a real policy guards, the one heo
// runs, so what the bridge may read is what it is granted: a missing
// action skips events, silently, as uma does for any subscriber.
type bridgeRig struct {
	*rig
	ch    model.ChannelRef
	inbox model.InboxRef
}

func newBridgeRig(t *testing.T, opts ...rigOption) *bridgeRig {
	t.Helper()
	r := newRig(t, "acme", opts...)
	r.deps.Authorizer = authz.New(authz.Config{Owner: []authz.Role{authz.RoleDesk}}, "acme", deskActor, daemon.ServiceActor)
	ch := r.channel("out", adapterstest.Config{Account: "acct-out"})
	inbox := r.inbox("out", ch, "desk")
	return &bridgeRig{rig: r, ch: ch, inbox: inbox}
}

// desk runs fn as the desk actor on the open runtime.
func (r *bridgeRig) desk(fn func(ctx context.Context, c uma.Client)) {
	r.t.Helper()
	err := r.host.Use(r.t.Context(), func(rt daemon.Runtime) error {
		c, err := rt.Engine.For(model.Principal{Tenant: r.tenant, Actor: deskActor})
		if err != nil {
			return err
		}
		fn(r.t.Context(), c)
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

// retry runs op until uma does not answer transient: the store asks a
// caller to rerun a unit that a concurrent one overtook, which the
// bridge's own reads of the log can cause.
func retry(t *testing.T, what string, op func() error) {
	t.Helper()
	var err error
	for range 50 {
		if err = op(); !model.IsCode(err, model.Transient) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// start opens a conversation with body, as the desk, and returns it.
func (r *bridgeRig) start(key, body string) model.ConversationRef {
	r.t.Helper()
	var conv model.ConversationRef
	r.desk(func(ctx context.Context, c uma.Client) {
		retry(r.t, "start", func() error {
			_, err := c.Conversations().Start(ctx, model.StartConversation{
				Command: r.cmd("start-" + key), Inbox: r.inbox,
				Recipient: model.ExternalRef{Channel: r.ch, Namespace: adapterstest.Namespace, Kind: "user", ID: "ada-" + key},
				Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: body}}},
			})
			return err
		})
		page, err := c.Conversations().List(ctx, model.ListRequest{
			Call:   model.Call{Correlation: "test"},
			Filter: model.Filter{Inboxes: []model.InboxRef{r.inbox}}, Page: model.PageRequest{Limit: 100},
		})
		if err != nil || len(page.Items) == 0 {
			r.t.Fatalf("list conversations: %v %d", err, len(page.Items))
		}
		conv = page.Items[len(page.Items)-1].Ref
	})
	return conv
}

func (r *bridgeRig) note(conv model.ConversationRef, key, text string) {
	r.t.Helper()
	r.desk(func(ctx context.Context, c uma.Client) {
		retry(r.t, "note", func() error {
			_, err := c.Messages().AddInternalNote(ctx, model.InternalNote{
				Command: r.cmd("note-" + key), Conversation: conv,
				Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
			})
			return err
		})
	})
}

// label creates a label named name, and adds it to conv and removes it.
func (r *bridgeRig) label(conv model.ConversationRef, key, name string) {
	r.t.Helper()
	r.desk(func(ctx context.Context, c uma.Client) {
		var acc model.Acceptance
		retry(r.t, "configure label", func() (err error) {
			acc, err = c.Labels().Configure(ctx, model.ConfigureLabel{
				Command: r.cmd("label-" + key),
				Label:   model.Label{Ref: model.LabelRef{Tenant: r.tenant}, Name: name},
			})
			return err
		})
		if acc.Operation.Result == nil {
			r.t.Fatal("label operation has no result")
		}
		ref := model.LabelRef{Tenant: acc.Operation.Result.Tenant, ID: acc.Operation.Result.ID}
		in := model.ConversationLabels{Command: r.cmd("add-" + key), Conversation: conv, Labels: []model.LabelRef{ref}}
		retry(r.t, "add label", func() error { _, err := c.Conversations().AddLabels(ctx, in); return err })
		in.Command = r.cmd("remove-" + key)
		retry(r.t, "remove label", func() error { _, err := c.Conversations().RemoveLabels(ctx, in); return err })
	})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("never: %s", what)
}

// TestBusBridgeCarriesNoBodies: a reply with a body, an internal note
// and a label with a name, committed while the bridge runs, reach the
// bus as references: not a byte of the body, the note's text or the
// label's name is in any payload a subscriber receives, in the bytes a
// file sink would write either; the note's events and the label's are
// desk-only, and the reply's are on the public topic. The bridge reads
// as its own actor, granted nothing but subscription reads, so this
// also shows those reads are enough: every event it was meant to carry
// arrived.
func TestBusBridgeCarriesNoBodies(t *testing.T) {
	const (
		body      = "BODY-SECRET-reply-text"
		noteText  = "NOTE-SECRET-internal-text"
		labelName = "LABEL-SECRET-name"
	)
	r := newBridgeRig(t)
	desk, public := listen(t, r.hub, bridge.ScopeDesk), listen(t, r.hub, bridge.ScopePublic)
	s := r.run(5*time.Second, daemon.NameBridge)
	s.ready(daemon.NameBridge)

	conv := r.start("a", body)
	r.note(conv, "a", noteText)
	r.label(conv, "a", labelName)

	eventually(t, "the reply, note and both label events on the bus", func() bool {
		return desk.count("uma.public.message.created") >= 1 && desk.count("uma.desk.message.created") >= 1 &&
			desk.count("uma.desk.conversation.label_added") >= 1 && desk.count("uma.desk.conversation.label_removed") >= 1
	})
	topics, raws := desk.all()
	for i, raw := range raws {
		for _, secret := range []string{body, noteText, labelName} {
			if strings.Contains(raw, secret) {
				t.Errorf("%s carries %q: %s", topics[i], secret, raw)
			}
		}
	}
	// The reply's references: the message and its conversation.
	created := desk.payloads("uma.public.message.created")
	if len(created) != 1 || created[0].Conversation != string(conv.ID) || created[0].Message == "" ||
		created[0].Resource.Kind != "message" || created[0].Operation == "" {
		t.Errorf("reply payload = %+v", created)
	}
	// The note is desk only, whatever the subscriber's patterns.
	notes := desk.payloads("uma.desk.message.created")
	if len(notes) != 1 || notes[0].Conversation != string(conv.ID) {
		t.Errorf("note payloads = %+v", notes)
	}
	labels := desk.payloads("uma.desk.conversation.label_added")
	if len(labels) != 1 || labels[0].Label == "" || labels[0].Conversation != string(conv.ID) {
		t.Errorf("label payloads = %+v", labels)
	}
	eventually(t, "the public subscriber to have received the reply's events", func() bool {
		return public.count("uma.public.message.created") >= 1
	})
	pubTopics, pubRaws := public.all()
	for i, topic := range pubTopics {
		if !strings.HasPrefix(string(topic), "uma.public.") {
			t.Errorf("public subscriber received %s: %s", topic, pubRaws[i])
		}
	}
	if public.count("uma.desk.message.created") != 0 {
		t.Error("the internal note's event reached a public subscriber")
	}
	// Every public event the desk saw, the public subscriber saw.
	for _, topic := range topics {
		if strings.HasPrefix(string(topic), "uma.public.") && public.count(topic) != desk.count(topic) {
			t.Errorf("%s: desk saw %d, public %d", topic, desk.count(topic), public.count(topic))
		}
	}
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
}

// TestBridgeDoesNotMoveAWatchCheckpoint: the bridge reads uma's log as a
// subscription of its own, so a desk watch keeps its own checkpoint and
// sees every event the bridge has already passed.
func TestBridgeDoesNotMoveAWatchCheckpoint(t *testing.T) {
	r := newBridgeRig(t)
	desk := listen(t, r.hub, bridge.ScopeDesk)
	s := r.run(5*time.Second, daemon.NameBridge)
	s.ready(daemon.NameBridge)

	var sub model.Subscription
	r.desk(func(ctx context.Context, c uma.Client) {
		res, err := c.Events().Open(ctx, model.OpenSubscription{
			Command:  model.Command{Call: model.Call{Correlation: "test"}, Operation: "watch-open"},
			Consumer: "heo-cli", Start: model.StartBeginning, PayloadMajor: model.PayloadVersion1,
		})
		if err != nil {
			t.Fatal(err)
		}
		sub = res.Subscription
	})
	conv := r.start("w", "hello")
	r.note(conv, "w", "a note")
	eventually(t, "the bridge to pass the events", func() bool {
		return desk.count("uma.public.message.created") >= 1 && desk.count("uma.desk.message.created") >= 1
	})
	eventually(t, "the bridge to acknowledge them", func() bool {
		st := r.host.Status().Bridge
		return st != nil && st.Checkpoint != "" && st.Published >= 2
	})
	r.desk(func(ctx context.Context, c uma.Client) {
		b, err := c.Events().Poll(ctx, model.Poll{Call: model.Call{Correlation: "test"}, Subscription: sub.Ref, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		kinds := map[string]int{}
		for _, ev := range b.Events {
			kinds[ev.Kind+"/"+string(ev.Audience.Visibility)]++
		}
		if kinds["message.created/public"] != 1 || kinds["message.created/internal"] != 1 {
			t.Errorf("the watch saw %v after the bridge passed the events", kinds)
		}
	})
}

// TestBridgeResumesWithoutLoss: a bridge stopped and started again
// resumes at its checkpoint: what was published and acknowledged before
// the stop is not published again, what was committed while it was down
// is published after.
func TestBridgeResumesWithoutLoss(t *testing.T) {
	r := newBridgeRig(t)
	desk := listen(t, r.hub, bridge.ScopeDesk)
	s := r.run(5*time.Second, daemon.NameBridge)
	s.ready(daemon.NameBridge)
	first := r.start("one", "first")
	eventually(t, "the first conversation's events", func() bool { return desk.count("uma.public.message.created") == 1 })
	eventually(t, "the checkpoint to move", func() bool {
		st := r.host.Status().Bridge
		return st != nil && st.Checkpoint != ""
	})
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
	if st := r.host.Status(); st.Bridge != nil {
		t.Errorf("a stopped bridge reports %+v", st.Bridge)
	}

	// Committed while the bridge is down.
	r.seedDown("two", "second")

	s = r.run(5*time.Second, daemon.NameBridge)
	s.ready(daemon.NameBridge)
	eventually(t, "the conversation committed while down", func() bool { return desk.count("uma.public.message.created") == 2 })
	time.Sleep(200 * time.Millisecond)
	created := desk.payloads("uma.public.message.created")
	if len(created) != 2 {
		t.Fatalf("message.created published %d times, want 2: %+v", len(created), created)
	}
	if created[0].Conversation != string(first.ID) || created[1].Conversation == string(first.ID) {
		t.Errorf("conversations = %s, %s; the first was %s", created[0].Conversation, created[1].Conversation, first.ID)
	}
}

// seedDown commits a conversation with the daemon down, through a
// second engine on the same store.
func (r *bridgeRig) seedDown(key, body string) {
	r.t.Helper()
	c, err := r.seed.For(model.Principal{Tenant: r.tenant, Actor: deskActor})
	if err != nil {
		r.t.Fatal(err)
	}
	_, err = c.Conversations().Start(r.t.Context(), model.StartConversation{
		Command: r.cmd("start-" + key), Inbox: r.inbox,
		Recipient: model.ExternalRef{Channel: r.ch, Namespace: adapterstest.Namespace, Kind: "user", ID: "ada-" + key},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: body}}},
	})
	if err != nil {
		r.t.Fatalf("start: %v", err)
	}
}

// TestStuckSubscriberDoesNotStallTheBridge: a bus subscriber that never
// returns costs only its own events: the bridge publishes every event,
// acknowledges its batches and counts the drops.
func TestStuckSubscriberDoesNotStallTheBridge(t *testing.T) {
	r := newBridgeRig(t)
	r.hub = bridge.NewHub(bridge.WithQueue(2))
	hub := r.hub
	t.Cleanup(func() { _ = hub.Close(context.Background()) })
	r.host = rebuildHost(r.rig)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	cancel := r.hub.Subscribe(bridge.ScopeDesk, "#", func(bus.Event) { <-release })
	t.Cleanup(cancel)
	desk := listen(t, r.hub, bridge.ScopeDesk)
	s := r.run(5*time.Second, daemon.NameBridge)
	s.ready(daemon.NameBridge)
	conv := r.start("s", "hello")
	const notes = 25
	for i := range notes {
		r.note(conv, fmt.Sprintf("s%d", i), "n")
	}
	eventually(t, "the bridge to publish every event and pass them", func() bool {
		st := r.host.Status().Bridge
		return st != nil && st.Published >= notes+1 && st.Checkpoint != ""
	})
	if desk.count("uma.desk.message.created") == 0 {
		t.Error("a healthy subscriber received nothing beside the stuck one")
	}
	if st := r.host.Status().Bridge; st.Dropped == 0 || st.State != bridge.StateReady {
		t.Errorf("status = %+v, want drops counted and the bridge ready", st)
	}
}

// TestBridgeStatusAndScope: the bridge appears in the runtime's status,
// ready with its subscription as its scope, and appears in the store
// owner's run like any runtime service.
func TestBridgeStatusAndScope(t *testing.T) {
	r := newBridgeRig(t)
	if st := r.host.Status(); st.Bridge != nil {
		t.Fatalf("bridge before start = %+v", st.Bridge)
	}
	s := r.run(5*time.Second, daemon.NameBridge)
	s.ready(daemon.NameBridge)
	st := r.host.Status()
	var svc *daemon.ServiceStatus
	for i := range st.Services {
		if st.Services[i].Name == daemon.NameBridge {
			svc = &st.Services[i]
		}
	}
	if svc == nil || svc.State != daemon.StateReady || len(svc.Scopes) != 1 || svc.Scopes[0].Scope != daemon.BridgeScope ||
		svc.Scopes[0].State != daemon.ScopeReady {
		t.Fatalf("service = %+v", svc)
	}
	if !st.Ready || st.Bridge == nil || st.Bridge.Consumer != bridge.Consumer || st.Bridge.State != bridge.StateReady {
		t.Fatalf("status = %+v bridge %+v", st, st.Bridge)
	}
	if r.storage.closes.Load() != 0 {
		t.Error("store closed while the bridge runs")
	}
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
	if r.storage.closes.Load() != 1 {
		t.Errorf("store closed %d times after the bridge stopped", r.storage.closes.Load())
	}
}

// TestBridgeStopsLastAmongRuntimeServices: with the others, the bridge
// is the last to stop, so what workers commit while draining is still
// read, and the store is released once.
func TestBridgeStopsLastAmongRuntimeServices(t *testing.T) {
	r := newBridgeRig(t)
	names := []string{daemon.NameWorkers, daemon.NameReceivers, daemon.NameBridge}
	var order []string
	for _, svc := range r.host.Services() {
		order = append(order, svc.Name())
	}
	if order[0] != daemon.NameBridge {
		t.Errorf("registration order = %v: the bridge registers first, so it stops last", order)
	}
	s := r.run(5*time.Second, names...)
	s.ready(names...)
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
	if r.storage.closes.Load() != 1 {
		t.Errorf("store closed %d times", r.storage.closes.Load())
	}
}

// TestBridgeNeedsABus: a daemon built without a bus does not start the
// bridge, and says why; the other services are unaffected.
func TestBridgeNeedsABus(t *testing.T) {
	r := newBridgeRig(t)
	r.hub = nil
	r.host = rebuildHost(r.rig)
	svc := r.host.Bridge()
	if err := svc.Validate(); err == nil || !strings.Contains(err.Error(), "no event bus") {
		t.Errorf("validate = %v", err)
	}
	if err := svc.Start(t.Context(), func() { t.Error("ready without a bus") }); err == nil || !strings.Contains(err.Error(), "no event bus") {
		t.Errorf("start = %v", err)
	}
	if r.storage.closes.Load() != 0 || r.host.StorageReady() {
		t.Error("the store was opened for a bridge that cannot run")
	}
}

// TestBridgeEnabledByDefault: services.bridge.enabled is on unless
// configured otherwise.
func TestBridgeEnabledByDefault(t *testing.T) {
	if v, ok := daemon.Defaults()["services."+daemon.NameBridge+".enabled"]; v != true || !ok {
		t.Errorf("services.bridge.enabled default = %v", v)
	}
}
