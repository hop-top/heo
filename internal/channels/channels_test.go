package channels_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/credentials"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/memory"
)

const (
	tenant   model.TenantID = "acme"
	operator model.ActorID  = "uid:1001"
	viewer   model.ActorID  = "uid:1002"
	desk     model.ActorID  = "uid:1003"
)

// world is one tenant's engine over an in-memory store, decided by
// heo's policy, with the fake adapter registered.
type world struct {
	t   *testing.T
	e   uma.Engine
	reg *adapters.Registry
	n   int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	reg := adapters.NewRegistry()
	if err := reg.Register(adapterstest.New().Adapter()); err != nil {
		t.Fatal(err)
	}
	return newWorldWith(t, reg)
}

// newWorldWith is a world running reg's adapters.
func newWorldWith(t *testing.T, reg *adapters.Registry) *world {
	t.Helper()
	pol := authz.New(authz.Config{
		Owner: []authz.Role{authz.RoleOperator},
		Grants: []authz.Grant{
			{Actor: viewer, Roles: []authz.Role{authz.RoleViewer}},
			{Actor: desk, Roles: []authz.Role{authz.RoleDesk}},
		},
	}, tenant, operator, "svc:heo-runtime")
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0))
	e, err := uma.New(uma.Config{
		Store: memory.New(memory.Options{Clock: clock}), Authorizer: pol,
		Adapters: reg.Factories(), Clock: clock, IDs: portstest.NewSequenceIDs(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return &world{t: t, e: e, reg: reg}
}

// as is actor's service.
func (w *world) as(actor model.ActorID) channels.Service {
	w.t.Helper()
	c, err := w.e.For(model.Principal{Tenant: tenant, Actor: actor})
	if err != nil {
		w.t.Fatal(err)
	}
	return channels.Service{Engine: w.e, Client: c, Tenant: tenant, Adapters: w.reg}
}

func (w *world) op() model.OperationID {
	w.n++
	return model.OperationID("op-" + strings.Repeat("x", w.n))
}

func ptr[T any](v T) *T { return &v }

func fakeConfig(t *testing.T, c adapterstest.Config) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// createChannel creates a fake channel with key on account.
func (w *world) createChannel(key, account string, denied ...model.Capability) model.Channel {
	w.t.Helper()
	s := w.as(operator)
	p, err := s.PlanChannelCreate(context.Background(), channels.ChannelChange{
		Type: adapterstest.Type, Key: ptr(key),
		Configuration: fakeConfig(w.t, adapterstest.Config{Account: account, Denied: denied}),
		CredentialRef: ptr("cred-" + key),
	}, w.op(), channels.Preview)
	if err != nil {
		w.t.Fatalf("plan channel %s: %v", key, err)
	}
	ch, acc, err := s.ConfigureChannel(context.Background(), p)
	if err != nil {
		w.t.Fatalf("create channel %s: %v", key, err)
	}
	if acc.Operation.State != model.Committed {
		w.t.Fatalf("create channel %s: operation %s", key, acc.Operation.State)
	}
	return ch
}

func (w *world) createInbox(channel, key string, dests ...string) model.Inbox {
	w.t.Helper()
	s := w.as(operator)
	p, err := s.PlanInboxCreate(context.Background(), channels.InboxChange{
		Channel: channel, Key: ptr(key), Name: ptr(key), Destinations: &dests, Mode: ptr(model.ModePerRoom),
	}, w.op(), channels.Preview)
	if err != nil {
		w.t.Fatalf("plan inbox %s: %v", key, err)
	}
	in, _, err := s.ConfigureInbox(context.Background(), p)
	if err != nil {
		w.t.Fatalf("create inbox %s: %v", key, err)
	}
	return in
}

func wantCode(t *testing.T, err error, code model.ErrorCode, contains string) {
	t.Helper()
	if !model.IsCode(err, code) || !strings.Contains(err.Error(), contains) {
		t.Fatalf("err = %v, want %s containing %q", err, code, contains)
	}
}

// TestChannelCreateFindUpdateAtRevision: a created channel is found by
// key and by ID; an update names the revision it replaces, and a stale
// one is a conflict that writes nothing.
func TestChannelCreateFindUpdateAtRevision(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	ch := w.createChannel("support", "bot-1")
	if ch.Key != "support" || ch.Type != adapterstest.Type || ch.CredentialRef != "cred-support" || ch.Revision == "" {
		t.Fatalf("created = %+v", ch)
	}
	if ch.Configuration.Name != adapterstest.Schema || ch.Configuration.Version != 1 {
		t.Errorf("configuration schema = %s/%d, want the adapter's", ch.Configuration.Name, ch.Configuration.Version)
	}
	s := w.as(operator)
	for _, ref := range []string{"support", string(ch.Ref.ID)} {
		got, err := s.FindChannel(ctx, ref)
		if err != nil || got.Ref != ch.Ref {
			t.Fatalf("find %s = %+v, %v", ref, got.Ref, err)
		}
	}
	_, err := s.FindChannel(ctx, "nope")
	wantCode(t, err, model.NotFound, `channel "nope"`)

	change := channels.ChannelChange{CredentialRef: ptr("cred-2")}
	_, err = s.PlanChannelUpdate(ctx, "support", "stale", change, w.op(), channels.Preview)
	wantCode(t, err, model.Conflict, "not stale")

	p, err := s.PlanChannelUpdate(ctx, "support", ch.Revision, change, w.op(), channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	if p.Current == nil || p.Current.CredentialRef != "cred-support" || p.Request.Channel.CredentialRef != "cred-2" ||
		p.Request.ExpectedRevision != ch.Revision {
		t.Fatalf("plan = %+v", p)
	}
	updated, _, err := s.ConfigureChannel(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CredentialRef != "cred-2" || updated.Revision == ch.Revision {
		t.Fatalf("updated = %+v", updated)
	}
	// Submitted at the old revision under a new key, the update passes
	// heo's plan, which leaves the revision to uma, and uma refuses it;
	// the refusal names the record and its revision.
	stale, err := s.PlanChannelUpdate(ctx, "support", ch.Revision, channels.ChannelChange{CredentialRef: ptr("cred-3")}, w.op(), channels.Submit)
	if err != nil {
		t.Fatalf("submission plan at the old revision: %v", err)
	}
	_, _, err = s.ConfigureChannel(ctx, stale)
	wantCode(t, err, model.Conflict, "channel support is at revision "+string(updated.Revision))
	if got, _ := s.FindChannel(ctx, "support"); got.CredentialRef != "cred-2" {
		t.Fatalf("stale update wrote %q", got.CredentialRef)
	}
	_, err = s.PlanChannelUpdate(ctx, "support", updated.Revision, channels.ChannelChange{}, w.op(), channels.Preview)
	wantCode(t, err, model.Validation, "nothing to change")
	_, err = s.PlanChannelUpdate(ctx, "support", updated.Revision, channels.ChannelChange{Type: "other"}, w.op(), channels.Preview)
	wantCode(t, err, model.Validation, "never changes type")
}

// TestChannelPlanChecksAndWritesNothing: a plan runs the checks the
// submission makes — adapter validation, key and provider account
// uniqueness — and writes nothing.
func TestChannelPlanChecksAndWritesNothing(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.createChannel("one", "bot-1")
	s := w.as(operator)
	plan := func(key, account string) error {
		_, err := s.PlanChannelCreate(ctx, channels.ChannelChange{
			Type: adapterstest.Type, Key: ptr(key), Configuration: fakeConfig(t, adapterstest.Config{Account: account}),
		}, w.op(), channels.Preview)
		return err
	}
	wantCode(t, plan("one", "bot-2"), model.Conflict, "channel.key")
	wantCode(t, plan("two", "bot-1"), model.Conflict, "channel one already receives this provider account")
	wantCode(t, plan("two", ""), model.Validation, "channel.account")
	_, err := s.PlanChannelCreate(ctx, channels.ChannelChange{Type: "nope.adapter"}, w.op(), channels.Preview)
	wantCode(t, err, model.Unsupported, "")
	_, err = s.PlanChannelCreate(ctx, channels.ChannelChange{Type: adapterstest.Type, Configuration: json.RawMessage(`[1]`)}, w.op(), channels.Preview)
	wantCode(t, err, model.Validation, "JSON object")

	p, err := s.PlanChannelCreate(ctx, channels.ChannelChange{
		Type: adapterstest.Type, Key: ptr("two"), Configuration: fakeConfig(t, adapterstest.Config{Account: "bot-2"}),
	}, w.op(), channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("warnings = %v, want none: the running daemon takes the channel as it commits", p.Warnings)
	}
	page, err := s.ListChannels(ctx, model.PageRequest{Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("channels after plans = %d, %v; want only the first", len(page.Items), err)
	}
}

// TestWritesSignalCommit: every channel and inbox submission runs the
// commit hook once uma answers, with the write already visible to it;
// the hook's error never fails the write, and a refused submission
// runs it too.
func TestWritesSignalCommit(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	s := w.as(operator)
	var seen []int
	s.Committed = func(ctx context.Context) error {
		page, err := s.ListChannels(ctx, model.PageRequest{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, len(page.Items))
		return errors.New("reconcile failed")
	}
	p, err := s.PlanChannelCreate(ctx, channels.ChannelChange{
		Type: adapterstest.Type, Key: ptr("one"), Configuration: fakeConfig(t, adapterstest.Config{Account: "bot-1"}),
	}, w.op(), channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	ch, _, err := s.ConfigureChannel(ctx, p)
	if err != nil {
		t.Fatalf("a failing hook failed the write: %v", err)
	}
	if !slices.Equal(seen, []int{1}) {
		t.Fatalf("hook saw %v, want the committed channel once", seen)
	}
	ip, err := s.PlanInboxCreate(ctx, channels.InboxChange{
		Channel: "one", Key: ptr("in"), Destinations: &[]string{"d"}, Mode: ptr(model.ModePerRoom),
	}, w.op(), channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ConfigureInbox(ctx, ip); err != nil {
		t.Fatal(err)
	}
	stale, err := s.PlanChannelUpdate(ctx, "one", ch.Revision, channels.ChannelChange{Key: ptr("uno")}, w.op(), channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	upd, err := s.PlanChannelUpdate(ctx, "one", ch.Revision, channels.ChannelChange{Key: ptr("eins")}, w.op(), channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ConfigureChannel(ctx, upd); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ConfigureChannel(ctx, stale); !model.IsCode(err, model.Conflict) {
		t.Fatalf("stale submission = %v, want conflict", err)
	}
	if len(seen) != 4 {
		t.Errorf("hook ran %d times over four submissions", len(seen))
	}
}

// TestInboxLifecycle: inboxes are created on a channel, destinations
// stay unique per channel, notes need an adapter declaring them, and
// disable flips Disabled alone at the named revision.
func TestInboxLifecycle(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	ch := w.createChannel("support", "bot-1")
	in := w.createInbox("support", "web", "site-1")
	if in.Channel != ch.Ref || !slices.Equal(in.Destinations, []string{"site-1"}) || in.Disabled {
		t.Fatalf("inbox = %+v", in)
	}
	s := w.as(operator)
	_, err := s.PlanInboxCreate(ctx, channels.InboxChange{
		Channel: "support", Name: ptr("dup"), Destinations: &[]string{"site-1"}, Mode: ptr(model.ModePerRoom),
	}, w.op(), channels.Preview)
	wantCode(t, err, model.Conflict, `destination "site-1" already bound to inbox web`)
	_, err = s.PlanInboxCreate(ctx, channels.InboxChange{
		Channel: "support", Name: ptr("notes"), Mode: ptr(model.ModePerRoom), DeliverNotes: ptr(true),
	}, w.op(), channels.Preview)
	wantCode(t, err, model.Unsupported, "internal_note")
	_, err = s.PlanInboxCreate(ctx, channels.InboxChange{Channel: "support", Name: ptr("x"), Mode: ptr(model.ModePerContact)}, w.op(), channels.Preview)
	wantCode(t, err, model.Validation, "lock_to_single")
	_, err = s.PlanInboxCreate(ctx, channels.InboxChange{Channel: "nope", Name: ptr("x"), Mode: ptr(model.ModePerRoom)}, w.op(), channels.Preview)
	wantCode(t, err, model.NotFound, "nope")

	_, err = s.PlanInboxUpdate(ctx, "web", "stale", channels.InboxChange{Disabled: ptr(true)}, w.op(), channels.Preview)
	wantCode(t, err, model.Conflict, "inbox web is at revision")
	p, err := s.PlanInboxUpdate(ctx, "web", in.Revision, channels.InboxChange{Disabled: ptr(true)}, w.op(), channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	off, _, err := s.ConfigureInbox(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !off.Disabled || off.Name != in.Name || !slices.Equal(off.Destinations, in.Destinations) || off.Revision == in.Revision {
		t.Fatalf("disabled inbox = %+v", off)
	}

	// Into per-contact mode, then out of it: the per-contact settings go
	// with it.
	p, err = s.PlanInboxUpdate(ctx, string(off.Ref.ID), off.Revision, channels.InboxChange{
		Mode: ptr(model.ModePerContact), LockToSingle: ptr(true), LifecycleRules: ptr(model.RuleSetContactLifecycleV1),
	}, w.op(), channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	pc, _, err := s.ConfigureInbox(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.PlanInboxUpdate(ctx, "web", pc.Revision, channels.InboxChange{Mode: ptr(model.ModePerThread)}, w.op(), channels.Preview)
	if err != nil {
		t.Fatalf("leaving per-contact mode: %v", err)
	}
	if sel := p.Request.Inbox.Selection; sel.LockToSingle != nil || sel.LifecycleRules != "" {
		t.Errorf("per-thread selection = %+v", sel)
	}
}

// TestRolesReachWhatThePolicyGrants: uma decides every call for the
// caller — a viewer configures nothing and reads no channel.
func TestRolesReachWhatThePolicyGrants(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	ch := w.createChannel("support", "bot-1")
	w.createInbox("support", "web", "site-1")
	v := w.as(viewer)
	_, err := v.PlanChannelCreate(ctx, channels.ChannelChange{
		Type: adapterstest.Type, Configuration: fakeConfig(t, adapterstest.Config{Account: "bot-9"}),
	}, w.op(), channels.Preview)
	if err == nil {
		// The plan reads nothing the viewer may not read; uma refuses
		// the write itself.
		p, _ := v.PlanChannelCreate(ctx, channels.ChannelChange{
			Type: adapterstest.Type, Configuration: fakeConfig(t, adapterstest.Config{Account: "bot-9"}),
		}, w.op(), channels.Preview)
		_, _, err = v.ConfigureChannel(ctx, p)
	}
	wantCode(t, err, model.Unauthorized, "")
	page, err := v.ListChannels(ctx, model.PageRequest{Limit: 10})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("viewer channel list = %d, %v", len(page.Items), err)
	}
	if _, err := v.FindInbox(ctx, "web"); err != nil {
		t.Fatalf("viewer inbox get: %v", err)
	}
	_, err = v.Check(ctx, string(ch.Ref.ID), channels.CheckOptions{Remote: true})
	if !model.IsCode(err, model.NotFound) && !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("viewer remote check = %v, want refused", err)
	}
}

// diagnoser reports every purpose missing but "graph".
type diagnoser struct{ seen []credentials.Request }

func (d *diagnoser) Check(_ context.Context, req credentials.Request) credentials.Diagnosis {
	d.seen = append(d.seen, req)
	st := credentials.StatusMissing
	if req.Purpose == "graph" {
		st = credentials.StatusOK
	}
	return credentials.Diagnosis{Reference: req.Reference, Purpose: req.Purpose, Status: st, Message: string(st)}
}

// TestCheckLocalAndRemote: the local check reads configuration only and
// reports what the channel's inboxes make of it; the remote check
// intersects the adapter's capabilities with the provider's permissions
// and the channel's needs, and diagnoses each credential purpose.
func TestCheckLocalAndRemote(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.createChannel("denied", "bot-1", model.CapabilitySend)
	w.createChannel("lonely", "bot-2")
	in := w.createInbox("denied", "web", "site-1")
	s := w.as(operator)
	p, err := s.PlanInboxUpdate(ctx, "web", in.Revision, channels.InboxChange{Disabled: ptr(true)}, w.op(), channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ConfigureInbox(ctx, p); err != nil {
		t.Fatal(err)
	}

	all, err := s.Check(ctx, "", channels.CheckOptions{})
	if err != nil || len(all) != 2 {
		t.Fatalf("check all = %d, %v", len(all), err)
	}
	byKey := map[string]channels.ChannelCheck{}
	for _, c := range all {
		byKey[c.Key] = c
		if c.Remote != nil {
			t.Errorf("%s: local check probed remotely", c.Key)
		}
		if !c.Adapter.Installed || c.Account == "" {
			t.Errorf("%s: adapter %+v account %q", c.Key, c.Adapter, c.Account)
		}
	}
	if c := byKey["lonely"]; !c.OK || !hasFinding(c, channels.SeverityWarning, "channel", "no_inbox") {
		t.Errorf("lonely = %+v", c)
	}
	if c := byKey["denied"]; !c.OK || !hasFinding(c, channels.SeverityWarning, "inbox:web", "disabled") || len(c.Inboxes) != 1 {
		t.Errorf("denied (local) = %+v", c)
	}

	// Disabled, the inbox needs no receiving, but replies still need
	// send, which the provider denies.
	d := &diagnoser{}
	got, err := s.Check(ctx, "denied", channels.CheckOptions{Remote: true, Credentials: d})
	if err != nil || len(got) != 1 {
		t.Fatalf("remote check = %+v, %v", got, err)
	}
	c := got[0]
	if c.Remote == nil || c.OK || !hasFinding(c, channels.SeverityError, "capability:send", string(model.Unauthorized)) {
		t.Fatalf("remote = %+v", c)
	}
	caps := map[model.Capability]channels.Capability{}
	for _, cp := range c.Remote.Capabilities {
		caps[cp.Name] = cp
	}
	if cp := caps[model.CapabilitySend]; cp.Available || !cp.Required {
		t.Errorf("send = %+v", cp)
	}
	if cp := caps[model.CapabilityReceive]; !cp.Available || cp.Required {
		t.Errorf("receive = %+v (the only inbox is disabled)", cp)
	}
	// The fake resolves no credential: nothing is diagnosed.
	if len(d.seen) != 0 || len(c.Remote.Credentials) != 0 {
		t.Errorf("diagnosed %v", d.seen)
	}
	_, err = s.Check(ctx, "", channels.CheckOptions{Remote: true})
	wantCode(t, err, model.Validation, "name it")
}

// TestCheckRemoteDiagnosesEachPurpose: every credential purpose of the
// channel's adapter is diagnosed under the channel's reference.
func TestCheckRemoteDiagnosesEachPurpose(t *testing.T) {
	w := newWorld(t)
	// The fake with credential purposes, as a real adapter declares them.
	reg := adapters.NewRegistry()
	a := adapterstest.New().Adapter()
	a.CredentialPurposes = []string{"graph", "webhook"}
	if err := reg.Register(a); err != nil {
		t.Fatal(err)
	}
	w.reg = reg
	w.createChannel("mail", "bot-1")
	w.createInbox("mail", "box", "a@example.com")
	d := &diagnoser{}
	got, err := w.as(operator).Check(context.Background(), "mail", channels.CheckOptions{Remote: true, Credentials: d})
	if err != nil {
		t.Fatal(err)
	}
	c := got[0]
	if len(d.seen) != 2 || d.seen[0].Reference != "cred-mail" || d.seen[1].Purpose != "webhook" {
		t.Fatalf("diagnosed %+v", d.seen)
	}
	if c.OK || !hasFinding(c, channels.SeverityError, "credential:webhook", string(credentials.StatusMissing)) ||
		hasFinding(c, channels.SeverityError, "credential:graph", string(credentials.StatusMissing)) {
		t.Errorf("findings = %+v", c.Findings)
	}
}

// TestCheckAdapterMissing: a channel whose adapter this build lacks is
// an error, found without opening anything.
func TestCheckAdapterMissing(t *testing.T) {
	w := newWorld(t)
	w.createChannel("orphan", "bot-1")
	w.reg = adapters.NewRegistry()
	got, err := w.as(operator).Check(context.Background(), "orphan", channels.CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if c := got[0]; c.OK || c.Adapter.Installed || !hasFinding(c, channels.SeverityError, "channel", "adapter_missing") {
		t.Errorf("check = %+v", c)
	}
}

func hasFinding(c channels.ChannelCheck, sev, scope, code string) bool {
	for _, f := range c.Findings {
		if f.Severity == sev && f.Scope == scope && f.Code == code {
			return true
		}
	}
	return false
}

var _ adapter.Factory = (*adapterstest.Factory)(nil)

// TestSubmitLeavesUmaConflictsToUma: a retry under the same operation
// key after a lost answer gets the stored operation back (replayed),
// though its key, its revision and its destinations now meet its own
// first attempt; a different key still meets uma's conflict. A preview
// holds every conflict final.
func TestSubmitLeavesUmaConflictsToUma(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	s := w.as(operator)
	change := channels.ChannelChange{
		Type: adapterstest.Type, Key: ptr("support"),
		Configuration: fakeConfig(t, adapterstest.Config{Account: "bot-1"}), CredentialRef: ptr("cred"),
	}
	create := func(op model.OperationID, m channels.Mode) (model.Channel, model.Acceptance, error) {
		p, err := s.PlanChannelCreate(ctx, change, op, m)
		if err != nil {
			return model.Channel{}, model.Acceptance{}, err
		}
		return s.ConfigureChannel(ctx, p)
	}
	first, acc, err := create("k1", channels.Submit)
	if err != nil || acc.Replayed {
		t.Fatalf("first create = %+v, %v", acc, err)
	}
	again, acc, err := create("k1", channels.Submit)
	if err != nil || !acc.Replayed || again.Ref != first.Ref {
		t.Fatalf("retried create = %+v %+v, %v; want the first, replayed", again.Ref, acc, err)
	}
	if _, _, err := create("k2", channels.Submit); !model.IsCode(err, model.Conflict) {
		t.Fatalf("create under another key = %v, want conflict", err)
	}
	if _, err := s.PlanChannelCreate(ctx, change, "k1", channels.Preview); !model.IsCode(err, model.Conflict) {
		t.Fatalf("preview of the retry = %v, want the conflict it would meet now", err)
	}

	in := w.createInbox("support", "web", "site-1")
	disable := func(op model.OperationID, m channels.Mode) (model.Inbox, model.Acceptance, error) {
		p, err := s.PlanInboxUpdate(ctx, "web", in.Revision, channels.InboxChange{Disabled: ptr(true)}, op, m)
		if err != nil {
			return model.Inbox{}, model.Acceptance{}, err
		}
		return s.ConfigureInbox(ctx, p)
	}
	off, acc, err := disable("d1", channels.Submit)
	if err != nil || acc.Replayed || !off.Disabled {
		t.Fatalf("disable = %+v %+v, %v", off, acc, err)
	}
	if _, acc, err := disable("d1", channels.Submit); err != nil || !acc.Replayed {
		t.Fatalf("retried disable at the old revision = %+v, %v; want replayed", acc, err)
	}
	if _, _, err := disable("d2", channels.Submit); !model.IsCode(err, model.Conflict) {
		t.Fatalf("another disable at the old revision = %v, want uma's conflict", err)
	}
}

// conditionalWorld registers the fake with a "push" purpose its adapter
// needs only for the channel keyed "pushing".
func conditionalWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	reg := adapters.NewRegistry()
	a := adapterstest.New().Adapter()
	a.CredentialPurposes = []string{"graph", "push"}
	a.CredentialIf = map[string]adapters.CredentialCondition{
		"push": {Needed: func(ch model.Channel) bool { return ch.Key == "pushing" }, Effect: "no push subscription; polling only"},
	}
	if err := reg.Register(a); err != nil {
		t.Fatal(err)
	}
	w.reg = reg
	w.createChannel("pushing", "acct-1")
	w.createChannel("polling", "acct-2")
	return w
}

// TestCheckConditionalCredential: a credential the channel's own
// configuration makes necessary is an error when absent, in the local
// check as in the remote one, once; a channel whose configuration does
// not need it is not asked.
func TestCheckConditionalCredential(t *testing.T) {
	w := conditionalWorld(t)
	ctx := context.Background()
	for _, remote := range []bool{false, true} {
		d := &diagnoser{}
		got, err := w.as(operator).Check(ctx, "", channels.CheckOptions{Credentials: d, Remote: false})
		if remote {
			got, err = w.as(operator).Check(ctx, "pushing", channels.CheckOptions{Credentials: d, Remote: true})
		}
		if err != nil {
			t.Fatal(err)
		}
		by := map[string]channels.ChannelCheck{}
		for _, c := range got {
			by[c.Key] = c
		}
		c := by["pushing"]
		n := 0
		for _, f := range c.Findings {
			if f.Scope == "credential:push" {
				n++
				if f.Severity != channels.SeverityError || f.Code != string(credentials.StatusMissing) ||
					!strings.Contains(f.Message, "polling only") {
					t.Errorf("remote=%v finding = %+v", remote, f)
				}
			}
		}
		if c.OK || n != 1 {
			t.Errorf("remote=%v pushing: ok=%v, %d findings: %+v", remote, c.OK, n, c.Findings)
		}
		if !remote {
			p := by["polling"]
			for _, f := range p.Findings {
				if f.Scope == "credential:push" {
					t.Errorf("polling channel asked for push: %+v", f)
				}
			}
		}
	}
	// Without a diagnoser nothing is read.
	got, err := w.as(operator).Check(ctx, "pushing", channels.CheckOptions{})
	if err != nil || hasFinding(got[0], channels.SeverityError, "credential:push", string(credentials.StatusMissing)) {
		t.Errorf("no diagnoser: %+v, %v", got, err)
	}
}

// TestCheckRemoteSkipsUnneededConditionalCredential: a channel whose
// configuration does not need the credential is not faulted for it by
// the remote check.
func TestCheckRemoteSkipsUnneededConditionalCredential(t *testing.T) {
	w := conditionalWorld(t)
	d := &diagnoser{}
	got, err := w.as(operator).Check(context.Background(), "polling", channels.CheckOptions{Credentials: d, Remote: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got[0].Findings {
		if f.Scope == "credential:push" {
			t.Errorf("finding %+v", f)
		}
	}
	for _, r := range d.seen {
		if r.Purpose == "push" {
			t.Errorf("diagnosed push for a channel that does not need it")
		}
	}
}
