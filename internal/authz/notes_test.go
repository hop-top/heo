package authz_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/viper"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/authz"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/store/memory"
)

type sysClock struct{}

func (sysClock) Now() time.Time { return time.Now() }

type uuids struct{}

func (uuids) New(context.Context) (model.ID, error) {
	id, err := uuid.NewV7()
	return model.ID(id.String()), err
}

// desk is a uma engine on an in-memory store decided by heo's policy,
// with one conversation holding a public message and an internal note.
type desk struct {
	t    *testing.T
	e    uma.Engine
	conv model.ConversationRef
	note model.MessageRef
	pub  model.MessageRef
}

var call = model.Call{Correlation: "test"}

func cmd(op string) model.Command { return model.Command{Call: call, Operation: model.OperationID(op)} }

func (d *desk) as(actor model.ActorID) uma.Client {
	d.t.Helper()
	c, err := d.e.For(model.Principal{Tenant: tenant, Actor: actor})
	if err != nil {
		d.t.Fatal(err)
	}
	return c
}

func newDesk(t *testing.T) *desk {
	t.Helper()
	v := viper.New()
	v.Set(authz.KeyOwner, []any{})
	v.Set(authz.KeyGrants, grants(
		map[string]any{"actor": "op", "roles": []any{"operator"}},
		map[string]any{"actor": "agent", "roles": []any{"desk"}},
		map[string]any{"actor": "auditor", "roles": []any{"viewer"}},
		map[string]any{"actor": "visitor", "roles": []any{"external"}},
	))
	p, err := authz.FromConfig(v, tenant, owner, service)
	if err != nil {
		t.Fatal(err)
	}
	fake := adapterstest.New()
	e, err := uma.New(uma.Config{
		Store: memory.New(memory.Options{}), Authorizer: p,
		Adapters: []adapter.Factory{fake}, Clock: sysClock{}, IDs: uuids{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	d := &desk{t: t, e: e}
	ctx := t.Context()

	// The operator configures; the desk agent starts a conversation.
	op := d.as("op")
	chAcc, err := op.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{Command: cmd("channel"), Channel: model.Channel{
		Ref: model.ChannelRef{Tenant: tenant}, Key: "out", Type: adapterstest.Type,
		Configuration: adapterstest.Config{Account: "acct"}.Configuration(),
	}})
	if err != nil {
		t.Fatalf("configure channel: %v", err)
	}
	ch, _ := chAcc.Operation.Result.AsChannel()
	lock := true
	ibAcc, err := op.Configuration().ConfigureInbox(ctx, model.ConfigureInbox{Command: cmd("inbox"), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: tenant}, Channel: ch, Name: "out", Destinations: []string{"desk"},
		Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
	}})
	if err != nil {
		t.Fatalf("configure inbox: %v", err)
	}
	inbox, _ := ibAcc.Operation.Result.AsInbox()
	agent := d.as("agent")
	start, err := agent.Conversations().Start(ctx, model.StartConversation{
		Command: cmd("start"), Inbox: inbox,
		Recipient: model.ExternalRef{Channel: ch, Namespace: adapterstest.Namespace, Kind: "user", ID: "ada"},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "public hello"}}},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Workers deliver the start, rechecking the agent under the policy.
	started := d.settle(start.Operation.Ref)
	if started.State != model.ProviderAccepted || started.ResultMessage == nil {
		t.Fatalf("start = %s %+v", started.State, started.Failure)
	}
	d.pub = *started.ResultMessage
	d.conv = d.pub.Conversation
	note, err := agent.Messages().AddInternalNote(ctx, model.InternalNote{
		Command: cmd("note"), Conversation: d.conv,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "internal only"}}},
	})
	if err != nil {
		t.Fatalf("note: %v", err)
	}
	noted := d.settle(note.Operation.Ref)
	if noted.ResultMessage == nil {
		t.Fatalf("note = %s %+v", noted.State, noted.Failure)
	}
	d.note = *noted.ResultMessage
	return d
}

// settle runs the workers until op leaves the queue.
func (d *desk) settle(op model.OperationRef) model.Operation {
	d.t.Helper()
	ctx, cancel := context.WithCancel(d.t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = d.e.Runtime().Run(ctx, uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: service}, Call: call},
			uma.WorkerOptions{Owner: "test", Poll: 5 * time.Millisecond, Lease: 10 * time.Second})
	}()
	defer func() { cancel(); wg.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := d.as("agent").Operations().Get(ctx, model.Lookup[model.OperationRef]{Call: call, Ref: op})
		if err != nil {
			d.t.Fatal(err)
		}
		if got.State != model.Queued && got.State != model.RetryPending {
			return got
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("operation still %s", got.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func refs(ms []model.Message) map[model.ID]bool {
	out := map[model.ID]bool{}
	for _, m := range ms {
		out[m.Ref.ID] = true
	}
	return out
}

// TestInternalNotesDeskOnly: an internal note is for desk actors only,
// on every read path the runtime serves — message list and get, and
// event subscriptions — while the conversation's public
// message stays readable to viewer and external actors. The operator,
// no desk actor, reads no conversation content at all.
func TestInternalNotesDeskOnly(t *testing.T) {
	d := newDesk(t)
	ctx := t.Context()
	for _, tc := range []struct {
		actor      model.ActorID
		pub, note  bool
		noMessages bool
	}{
		{"agent", true, true, false},
		{"auditor", true, false, false},
		{"visitor", true, false, false},
		{"op", false, false, true},
	} {
		c := d.as(tc.actor)
		page, err := c.Messages().List(ctx, model.ListRequest{Call: call, Filter: model.Filter{Conversation: &d.conv}})
		if err != nil {
			t.Fatalf("%s: list: %v", tc.actor, err)
		}
		got := refs(page.Items)
		if got[d.pub.ID] != tc.pub || got[d.note.ID] != tc.note {
			t.Errorf("%s: message list = %v, want public %v note %v", tc.actor, got, tc.pub, tc.note)
		}
		_, err = c.Messages().Get(ctx, model.Lookup[model.MessageRef]{Call: call, Ref: d.note})
		if (err == nil) != tc.note {
			t.Errorf("%s: get note err = %v, want readable %v", tc.actor, err, tc.note)
		}
		if err != nil && !model.IsCode(err, model.NotFound) && !model.IsCode(err, model.Unauthorized) {
			t.Errorf("%s: get note err = %v, want not found or unauthorized", tc.actor, err)
		}

		// A subscription carries the note's events to desk actors only.
		sub, err := c.Events().Open(ctx, model.OpenSubscription{Command: cmd("sub-" + string(tc.actor)),
			Consumer: "test-" + string(tc.actor), Start: model.StartBeginning, PayloadMajor: model.PayloadVersion1})
		if err != nil {
			if tc.noMessages {
				continue
			}
			t.Fatalf("%s: open: %v", tc.actor, err)
		}
		batch, err := c.Events().Poll(ctx, model.Poll{Call: call, Subscription: sub.Subscription.Ref, Limit: 100})
		if err != nil {
			t.Fatalf("%s: poll: %v", tc.actor, err)
		}
		if seen := eventsAbout(batch.Events, d.note); seen != tc.note {
			t.Errorf("%s: subscription carries the note: %v, want %v", tc.actor, seen, tc.note)
		}
		if !tc.noMessages && !eventsAbout(batch.Events, d.pub) {
			t.Errorf("%s: subscription lacks the public message", tc.actor)
		}
	}
}

func eventsAbout(evs []model.Event, m model.MessageRef) bool {
	for _, ev := range evs {
		if ev.Resource == m.Ref() {
			return true
		}
	}
	return false
}
