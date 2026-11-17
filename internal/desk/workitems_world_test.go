package desk_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/adapter/ticket/gitlab"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// hookSecret is the GitHub webhook secret of the test channel.
const hookSecret = "webhook-test-secret" // #nosec G101 -- test value, not a credential

// sandboxRepo is the repository of uma's GitHub webhook fixtures.
const sandboxRepo = "example/uma-sandbox"

// The seeded Zendesk ticket's records.
const (
	ticketConv model.ID = "00000ad0-0000-7000-8000-0000000001c1"
	ticketItem model.ID = "00000ad0-0000-7000-8000-0000000001c2"
	ticketID            = "100"
)

// ticketWorld is one tenant's engine over an in-memory store, decided
// by heo's policy, with both ticket adapters registered as heo's
// composition registers them: a github.issues channel whose inbox
// binds uma's fixture repository, fed by uma's webhook fixtures through
// the adapter's own webhook handler, and a zendesk.tickets channel
// holding one ticket. No uma worker runs, so nothing reaches a
// provider: GitHub ingress needs no provider call, and uma has no
// recorded Zendesk exchanges to replay.
type ticketWorld struct {
	t      *testing.T
	e      uma.Engine
	st     store.Store
	pol    *authz.Policy
	hook   http.Handler
	gh     model.ChannelRef
	zd     model.ChannelRef
	gl     model.ChannelRef
	glIn   model.InboxRef
	gm     model.ChannelRef
	ms     model.ChannelRef
	gmIn   model.InboxRef
	msIn   model.InboxRef
	zdIn   model.InboxRef
	n      int
	ticket model.ConversationRef
}

// noNetwork is the HTTP client of the world's engine: the mail adapters
// open an instance only with one, and nothing here reaches a provider, so
// every request it is given fails.
type noNetwork struct{}

func (noNetwork) Do(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("no network in tests: %s %s", r.Method, r.URL.Host)
}

type hookCreds struct{}

func (hookCreds) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	if r.Purpose == github.CredentialPurposeWebhook {
		return ports.Secret{Bytes: []byte(hookSecret)}, nil
	}
	return nil, model.NewError(model.Unauthorized, "no credential for "+r.Purpose)
}

func newTicketWorld(t *testing.T) *ticketWorld {
	t.Helper()
	pol := authz.New(authz.Config{
		Owner: []authz.Role{authz.RoleOperator},
		Grants: []authz.Grant{
			{Actor: viewer, Roles: []authz.Role{authz.RoleViewer}},
			{Actor: agent, Roles: []authz.Role{authz.RoleDesk}},
			{Actor: agent2, Roles: []authz.Role{authz.RoleDesk}},
			{Actor: plugin, Roles: []authz.Role{authz.RoleDesk}},
		},
	}, tenant, operator, service)
	clock := portstest.NewFixedClock(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	st := memory.New(memory.Options{Clock: clock})
	zf := zendesk.NewFactory()
	lf := gitlab.NewFactory(gitlab.Options{})
	gf := github.NewFactory(github.Options{})
	mg := google.NewFactory(google.Options{})
	mm := microsoft.NewFactory(microsoft.Options{})
	e, err := uma.New(uma.Config{
		Store: st, Authorizer: pol,
		Adapters:   []adapter.Factory{gf, zf, lf, mg, mm},
		Extensions: slices.Concat(zf.Extensions(), gf.Extensions(), mg.Extensions(), mm.Extensions()), Credentials: hookCreds{},
		HTTP:  noNetwork{},
		Clock: clock, IDs: portstest.NewSequenceIDs(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	w := &ticketWorld{t: t, e: e, st: st, pol: pol}
	w.gh = w.channel("gh", github.Type, github.Config{}.Extension())
	w.inbox("issues", w.gh, sandboxRepo)
	w.zd = w.channel("zd", zendesk.Type, zendesk.Config{
		Subdomain: "acme", Auth: zendesk.AuthAPIToken, Email: "agent@example.com", WebhookToleranceSeconds: 300,
	}.Extension())
	w.zdIn = w.inbox("support", w.zd, "360000001")
	w.gl = w.channel("gl", gitlab.Type, gitlab.Config{}.Extension())
	w.glIn = w.inbox("gitlab", w.gl, "group/project")
	w.gm = w.channel("gm", google.Type, model.Extension{Name: google.ConfigSchema, Version: google.ConfigVersion, Data: []byte(`{"user":"me"}`)})
	w.gmIn = w.mailInbox("gmail", w.gm, "me")
	w.ms = w.channel("ms", microsoft.Type, model.Extension{Name: microsoft.ConfigSchema, Version: microsoft.ConfigVersion, Data: []byte(`{"mailbox":"me@example.com"}`)})
	w.msIn = w.mailInbox("outlook", w.ms, "me@example.com")
	if w.hook, err = e.Runtime().Webhook(t.Context(), uma.RuntimeCall{
		Principal: model.Principal{Tenant: tenant, Actor: service}, Call: call,
	}, w.gh); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	w.ticket = w.seedTicket()
	return w
}

func (w *ticketWorld) cmd() model.Command {
	w.n++
	return model.Command{Call: call, Operation: model.OperationID(fmt.Sprintf("tw-%d", w.n))}
}

func (w *ticketWorld) client(actor model.ActorID) uma.Client {
	w.t.Helper()
	c, err := w.e.For(model.Principal{Tenant: tenant, Actor: actor})
	if err != nil {
		w.t.Fatal(err)
	}
	return c
}

func (w *ticketWorld) as(actor model.ActorID) desk.Service {
	p := model.Principal{Tenant: tenant, Actor: actor}
	return desk.Service{Client: w.client(actor), Tenant: tenant, Sees: func(a model.Audience) bool { return w.pol.Sees(p, a) },
		Actor: actor, Permits: func(who model.ActorID, action string) bool {
			ok, _ := w.pol.Permits(model.Principal{Tenant: tenant, Actor: who}, action)
			return ok
		}}
}

// asKnowing is actor's desk with the runtime's view of the channels'
// adapters, which a preview's declaration check reads.
func (w *ticketWorld) asKnowing(actor model.ActorID) desk.Service {
	s := w.as(actor)
	s.Adapter = w.adapter
	s.Effects = w.effects
	s.ValidateAction = w.validate
	return s
}

// adapter describes the adapter of channel ref, as the runtime reads it
// from the binary.
func (w *ticketWorld) adapter(_ context.Context, ref model.ChannelRef) (adapter.Descriptor, error) {
	switch ref {
	case w.gh:
		return github.NewFactory(github.Options{}).Describe(), nil
	case w.zd:
		return zendesk.NewFactory().Describe(), nil
	case w.gl:
		return gitlab.NewFactory(gitlab.Options{}).Describe(), nil
	case w.gm:
		return google.NewFactory(google.Options{}).Describe(), nil
	case w.ms:
		return microsoft.NewFactory(microsoft.Options{}).Describe(), nil
	}
	return adapter.Descriptor{}, model.NewError(model.NotFound, "channel not found")
}

func (w *ticketWorld) channel(key string, typ model.TypeKey, cfg model.Extension) model.ChannelRef {
	w.t.Helper()
	acc, err := w.client(operator).Configuration().ConfigureChannel(w.t.Context(), model.ConfigureChannel{Command: w.cmd(), Channel: model.Channel{
		Ref: model.ChannelRef{Tenant: tenant}, Key: key, Type: typ, Configuration: cfg, CredentialRef: key,
	}})
	if err != nil {
		w.t.Fatalf("channel %s: %v", key, err)
	}
	ref, _ := acc.Operation.Result.AsChannel()
	return ref
}

func (w *ticketWorld) inbox(key string, ch model.ChannelRef, dest string) model.InboxRef {
	w.t.Helper()
	acc, err := w.client(operator).Configuration().ConfigureInbox(w.t.Context(), model.ConfigureInbox{Command: w.cmd(), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: tenant}, Channel: ch, Key: key, Name: key, Destinations: []string{dest},
		Selection: model.Selection{Mode: model.ModePerWorkItem},
	}})
	if err != nil {
		w.t.Fatalf("inbox %s: %v", key, err)
	}
	ref, _ := acc.Operation.Result.AsInbox()
	return ref
}

// moduleDir is the directory of module path in this build.
func moduleDir(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", path).Output() //nolint:gosec // fixed module path
	if err != nil {
		t.Fatalf("go list %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

// deliver posts uma's GitHub webhook fixture name as event, signed
// under the channel's webhook secret, through the adapter's handler.
func (w *ticketWorld) deliver(event, name, delivery string) {
	w.t.Helper()
	body, err := os.ReadFile(filepath.Join(moduleDir(w.t, "hop.top/uma/adapter/ticket/github"), "testdata", "webhooks", name+".json")) // #nosec G304 -- fixture
	if err != nil {
		w.t.Fatal(err)
	}
	m := hmac.New(sha256.New, []byte(hookSecret))
	m.Write(body)
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(github.HeaderEvent, event)
	r.Header.Set(github.HeaderDelivery, delivery)
	r.Header.Set(github.HeaderSignature, "sha256="+hex.EncodeToString(m.Sum(nil)))
	rec := httptest.NewRecorder()
	w.hook.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		w.t.Fatalf("%s %s = %d %s", event, name, rec.Code, rec.Body.String())
	}
}

// ticketRoute is the seeded ticket's provider reference.
func (w *ticketWorld) ticketRoute() model.ExternalRef {
	return model.ExternalRef{Channel: w.zd, Namespace: zendesk.Namespace, Kind: model.RefKindWorkItem, ID: ticketID}
}

// seedTicket writes one open Zendesk ticket and its conversation into
// the store, as ingress records them. uma holds no recorded Zendesk
// exchanges, so the ticket is seeded rather than received.
func (w *ticketWorld) seedTicket() model.ConversationRef {
	w.t.Helper()
	ctx := w.t.Context()
	route := w.ticketRoute()
	conv := model.ConversationRef{Inbox: w.zdIn, ID: ticketConv}
	requester := model.ExternalRef{Channel: w.zd, Namespace: zendesk.Namespace, Kind: zendesk.RefKindUser, ID: "42"}
	if err := w.st.Transact(ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutConversation(ctx, model.Conversation{
			Ref: conv, Mode: model.ModePerWorkItem, Status: model.ConversationOpen,
			External: []model.ExternalRef{route}, ReplyRoute: route,
			CreatedAt: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC), Revision: "00000ad0-0000-7000-8000-0000000001d1",
		}, ""); err != nil {
			return err
		}
		return tx.PutWorkItem(ctx, model.WorkItem{
			Ref: model.WorkItemRef{Conversation: conv, ID: ticketItem}, External: route,
			Subject: "My printer is on fire", Status: model.WorkItemOpen, RawStatus: "open",
			Requester: requester, Binding: model.WorkItemBindingActive,
			Source:   model.WorkItemSource{Revision: "1", OccurredAt: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)},
			Revision: "00000ad0-0000-7000-8000-0000000001d2",
		}, "")
	}); err != nil {
		w.t.Fatalf("seed ticket: %v", err)
	}
	return conv
}

// providerReports applies a status Zendesk reported for the ticket, as
// ingress applies the provider's own report of a change.
func (w *ticketWorld) providerReports(raw, status string) {
	w.t.Helper()
	ctx := w.t.Context()
	if err := w.st.Transact(ctx, tenant, func(tx store.Tx) error {
		cur, err := tx.GetWorkItem(ctx, model.WorkItemRef{Conversation: w.ticket, ID: ticketItem})
		if err != nil {
			return err
		}
		next := cur.Clone()
		next.RawStatus, next.Status, next.Revision = raw, status, "00000ad0-0000-7000-8000-0000000001d3"
		next.Source = model.WorkItemSource{Revision: "2", OccurredAt: time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)}
		return tx.PutWorkItem(ctx, next, cur.Revision)
	}); err != nil {
		w.t.Fatalf("provider report: %v", err)
	}
}

// issue ingests uma's fixture issue (opened, then commented) and
// returns its work item.
func (w *ticketWorld) issue() model.WorkItem {
	w.t.Helper()
	w.deliver("issues", "issues-opened", "d-opened")
	w.deliver("issue_comment", "issue_comment-created", "d-comment")
	p, err := w.client(agent).WorkItems().List(w.t.Context(), model.ListRequest{Call: call, Page: model.PageRequest{Limit: 100}})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, it := range p.Items {
		if it.External.Namespace == github.Namespace {
			return it
		}
	}
	w.t.Fatalf("no issue among %+v", p.Items)
	return model.WorkItem{}
}

func (w *ticketWorld) item() model.WorkItem {
	w.t.Helper()
	it, err := w.client(agent).WorkItems().Get(w.t.Context(), model.Lookup[model.WorkItemRef]{Call: call,
		Ref: model.WorkItemRef{Conversation: w.ticket, ID: ticketItem}})
	if err != nil {
		w.t.Fatal(err)
	}
	return it
}

func (w *ticketWorld) conversation(ref model.ConversationRef) model.Conversation {
	w.t.Helper()
	c, err := w.client(agent).Conversations().Get(w.t.Context(), model.Lookup[model.ConversationRef]{Call: call, Ref: ref})
	if err != nil {
		w.t.Fatal(err)
	}
	return c
}

// providerWork is uma's work kind for an operation a worker executes
// at the provider.
const providerWork = "operation.execute"

// queued counts the provider work waiting for a worker: what would
// reach a provider once one runs.
func (w *ticketWorld) queued() int {
	w.t.Helper()
	var n int
	if err := w.st.View(w.t.Context(), tenant, func(r store.Reader) error {
		d, err := r.WorkDepth(w.t.Context(), providerWork)
		n = d.Pending + d.Claimed + d.Quarantined
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// record is the stored operation ref.
func (w *ticketWorld) record(ref model.OperationRef) store.OperationRecord {
	w.t.Helper()
	var rec store.OperationRecord
	if err := w.st.View(w.t.Context(), tenant, func(r store.Reader) (err error) {
		rec, err = r.Operation(w.t.Context(), ref)
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return rec
}

// The seeded GitLab issue's records.
const (
	gitlabConv model.ID = "00000ad0-0000-7000-8000-0000000001f1"
	gitlabItem model.ID = "00000ad0-0000-7000-8000-0000000001f2"
)

// seedGitLabIssue writes one open GitLab issue and its conversation into
// the store, as ingress records them; localFields marks the fields uma
// owns. uma holds no recorded GitLab exchanges, so the issue is seeded
// rather than received.
func (w *ticketWorld) seedGitLabIssue(localFields ...string) model.WorkItem {
	w.t.Helper()
	ctx := w.t.Context()
	route := model.ExternalRef{Channel: w.gl, Namespace: gitlab.Namespace, Kind: model.RefKindWorkItem, ID: "5", ParentID: "group/project"}
	conv := model.ConversationRef{Inbox: w.glIn, ID: gitlabConv}
	item := model.WorkItem{
		Ref: model.WorkItemRef{Conversation: conv, ID: gitlabItem}, External: route,
		Subject: "Build is red", Status: model.WorkItemOpen, RawStatus: "opened",
		Binding: model.WorkItemBindingActive, LocalFields: localFields,
		Source:   model.WorkItemSource{Revision: "1", OccurredAt: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)},
		Revision: "00000ad0-0000-7000-8000-0000000001f3",
	}
	if err := w.st.Transact(ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutConversation(ctx, model.Conversation{
			Ref: conv, Mode: model.ModePerWorkItem, Status: model.ConversationOpen,
			External: []model.ExternalRef{route}, ReplyRoute: route,
			CreatedAt: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC), Revision: "00000ad0-0000-7000-8000-0000000001f4",
		}, ""); err != nil {
			return err
		}
		return tx.PutWorkItem(ctx, item, "")
	}); err != nil {
		w.t.Fatalf("seed gitlab issue: %v", err)
	}
	return item
}

// providerSets applies a status the provider reported for the item, as
// ingress applies the provider's own report of a change.
func (w *ticketWorld) providerSets(it model.WorkItem, status, raw string) {
	w.t.Helper()
	ctx := w.t.Context()
	if err := w.st.Transact(ctx, tenant, func(tx store.Tx) error {
		cur, err := tx.GetWorkItem(ctx, it.Ref)
		if err != nil {
			return err
		}
		next := cur.Clone()
		w.n++
		next.Status, next.RawStatus = status, raw
		next.Revision = model.Revision(fmt.Sprintf("00000ad0-0000-7000-8000-0000000005%02x", w.n))
		return tx.PutWorkItem(ctx, next, cur.Revision)
	}); err != nil {
		w.t.Fatalf("provider report: %v", err)
	}
}

// providerBinds applies a binding the provider reported for the item.
func (w *ticketWorld) providerBinds(it model.WorkItem, binding string) {
	w.t.Helper()
	ctx := w.t.Context()
	if err := w.st.Transact(ctx, tenant, func(tx store.Tx) error {
		cur, err := tx.GetWorkItem(ctx, it.Ref)
		if err != nil {
			return err
		}
		next := cur.Clone()
		w.n++
		next.Binding = binding
		next.Revision = model.Revision(fmt.Sprintf("00000ad0-0000-7000-8000-0000000006%02x", w.n))
		return tx.PutWorkItem(ctx, next, cur.Revision)
	}); err != nil {
		w.t.Fatalf("provider binding: %v", err)
	}
}
