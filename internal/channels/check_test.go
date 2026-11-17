package channels_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/composition"
	"hop.top/heo/internal/credentials"
	"hop.top/uma/adapter/chat/discord"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/adapter/chat/telegram"
	"hop.top/uma/model"
)

// purposeDiag reports every purpose missing but those in ok, and keeps
// the purposes asked, in order.
type purposeDiag struct {
	ok   []string
	seen []string
}

func (d *purposeDiag) Check(_ context.Context, req credentials.Request) credentials.Diagnosis {
	d.seen = append(d.seen, req.Purpose)
	st := credentials.StatusMissing
	if slices.Contains(d.ok, req.Purpose) {
		st = credentials.StatusOK
	}
	return credentials.Diagnosis{Reference: req.Reference, Purpose: req.Purpose, Status: st, Message: string(st)}
}

// createTyped creates a channel of typ with key and configuration cfg,
// under credential reference cred-<key>.
func (w *world) createTyped(key string, typ model.TypeKey, cfg any) model.Channel {
	w.t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		w.t.Fatal(err)
	}
	s := w.as(operator)
	p, err := s.PlanChannelCreate(context.Background(), channels.ChannelChange{
		Type: typ, Key: ptr(key), Configuration: b, CredentialRef: ptr("cred-" + key),
	}, w.op(), channels.Preview)
	if err != nil {
		w.t.Fatalf("plan channel %s: %v", key, err)
	}
	ch, _, err := s.ConfigureChannel(context.Background(), p)
	if err != nil {
		w.t.Fatalf("create channel %s: %v", key, err)
	}
	return ch
}

// findingFor is the finding scoped to scope, if any.
func findingFor(c channels.ChannelCheck, scope string) (channels.Finding, bool) {
	for _, f := range c.Findings {
		if f.Scope == scope {
			return f, true
		}
	}
	return channels.Finding{}, false
}

// TestCheckRemoteSkipsCredentialsOfSwitchedOffReceiving: a purpose only
// one way of receiving resolves is diagnosed unless the channel's
// configuration switches that way off (reported disabled), as the daemon
// then neither starts nor mounts it. Unavailable for another reason, the
// way is still run, and its credential still needed. A missing one says
// which way needs it.
func TestCheckRemoteSkipsCredentialsOfSwitchedOffReceiving(t *testing.T) {
	reg := adapters.NewRegistry()
	a := adapterstest.New().Adapter()
	a.CredentialPurposes = []string{"api", "signing", "socket"}
	a.CredentialReceiving = map[string]model.Capability{"signing": model.CapabilityReceiveHTTP, "socket": model.CapabilityReceive}
	if err := reg.Register(a); err != nil {
		t.Fatal(err)
	}
	w := newWorldWith(t, reg)
	w.createTyped("socket", adapterstest.Type, adapterstest.Config{Account: "a", Disabled: []model.Capability{model.CapabilityReceiveHTTP}})
	w.createTyped("url", adapterstest.Type, adapterstest.Config{Account: "b", Disabled: []model.Capability{model.CapabilityReceive}})
	w.createTyped("both", adapterstest.Type, adapterstest.Config{Account: "c"})
	w.createTyped("nopush", adapterstest.Type, adapterstest.Config{Account: "d", NoWebhook: true})
	for key, want := range map[string][]string{
		"socket": {"api", "socket"},
		"url":    {"api", "signing"},
		"both":   {"api", "signing", "socket"},
		"nopush": {"api", "signing", "socket"},
	} {
		d := &purposeDiag{ok: []string{"api"}}
		got, err := w.as(operator).Check(context.Background(), key, channels.CheckOptions{Remote: true, Credentials: d})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		c := got[0]
		if !slices.Equal(d.seen, want) || len(c.Remote.Credentials) != len(want) {
			t.Errorf("%s: diagnosed %v (%d reported), want %v", key, d.seen, len(c.Remote.Credentials), want)
		}
		for _, p := range []string{"signing", "socket"} {
			f, found := findingFor(c, "credential:"+p)
			if found != slices.Contains(want, p) {
				t.Errorf("%s: finding for %s = %v, want %v", key, p, found, slices.Contains(want, p))
			}
			if found && (f.Severity != channels.SeverityError || !strings.Contains(f.Message, "needed for "+string(a.CredentialReceiving[p]))) {
				t.Errorf("%s: %s finding = %+v", key, p, f)
			}
		}
		if _, found := findingFor(c, "credential:api"); found {
			t.Errorf("%s: a stored api credential reported: %+v", key, c.Findings)
		}
	}
}

// TestCheckRemoteSlackCredentialsPerReceiveMode: a slack.chat channel
// needs its bot token always, the signing secret when it takes the
// request URL and the app-level token when it takes Socket Mode. With
// receive empty both are offered, and the daemon starts the socket
// receiver and mounts the request URL, so both are needed.
func TestCheckRemoteSlackCredentialsPerReceiveMode(t *testing.T) {
	all, err := composition.Adapters(composition.Options{})
	if err != nil {
		t.Fatal(err)
	}
	reg := adapters.NewRegistry()
	for _, a := range all {
		if a.Factory.Describe().Type == slack.Type {
			if err := reg.Register(a); err != nil {
				t.Fatal(err)
			}
		}
	}
	w := newWorldWith(t, reg)
	api, signing, socket := slack.CredentialPurposeAPI, slack.CredentialPurposeSigning, slack.CredentialPurposeSocket
	for i, tc := range []struct {
		mode slack.ReceiveMode
		want []string
	}{
		{slack.ReceiveEventsAPI, []string{api, signing}},
		{slack.ReceiveSocketMode, []string{api, socket}},
		{"", []string{api, signing, socket}},
	} {
		key := "slack-" + string(tc.mode)
		w.createTyped(key, slack.Type, slack.Config{
			TeamID: "T000000000" + string(rune('1'+i)), BotUserID: "U0000000001", Receive: tc.mode,
		})
		d := &purposeDiag{ok: []string{api}}
		got, err := w.as(operator).Check(context.Background(), key, channels.CheckOptions{Remote: true, Credentials: d})
		if err != nil {
			t.Fatalf("receive %q: %v", tc.mode, err)
		}
		c := got[0]
		if !slices.Equal(d.seen, tc.want) {
			t.Errorf("receive %q: diagnosed %v, want %v", tc.mode, d.seen, tc.want)
		}
		for _, p := range []string{signing, socket} {
			if _, found := findingFor(c, "credential:"+p); found != slices.Contains(tc.want, p) {
				t.Errorf("receive %q: missing %s reported = %v, want %v; findings %+v", tc.mode, p, found, !found, c.Findings)
			}
		}
		if c.OK {
			t.Errorf("receive %q: OK with a needed credential missing", tc.mode)
		}
	}
}

// TestCheckRemoteTelegramWebhookSecretPerReceiveMode: a polled
// telegram.messages bot needs no webhook secret; one taking its webhook,
// or with receive empty (heo mounts the route), does.
func TestCheckRemoteTelegramWebhookSecretPerReceiveMode(t *testing.T) {
	all, err := composition.Adapters(composition.Options{})
	if err != nil {
		t.Fatal(err)
	}
	reg := adapters.NewRegistry()
	for _, a := range all {
		if a.Factory.Describe().Type == telegram.Type {
			if err := reg.Register(a); err != nil {
				t.Fatal(err)
			}
		}
	}
	w := newWorldWith(t, reg)
	api, webhook := telegram.CredentialPurposeAPI, telegram.CredentialPurposeWebhook
	for i, tc := range []struct {
		mode telegram.ReceiveMode
		want []string
	}{
		{telegram.ReceivePoll, []string{api}},
		{telegram.ReceiveWebhook, []string{api, webhook}},
		{"", []string{api, webhook}},
	} {
		key := "bot-" + string(tc.mode)
		w.createTyped(key, telegram.Type, telegram.Config{BotID: int64(100 + i), BotUsername: "acme_bot", Receive: tc.mode})
		d := &purposeDiag{ok: []string{api}}
		got, err := w.as(operator).Check(context.Background(), key, channels.CheckOptions{Remote: true, Credentials: d})
		if err != nil {
			t.Fatalf("receive %q: %v", tc.mode, err)
		}
		if !slices.Equal(d.seen, tc.want) {
			t.Errorf("receive %q: diagnosed %v, want %v", tc.mode, d.seen, tc.want)
		}
		if _, found := findingFor(got[0], "credential:"+webhook); found != slices.Contains(tc.want, webhook) {
			t.Errorf("receive %q: missing webhook secret reported = %v; findings %+v", tc.mode, found, got[0].Findings)
		}
	}
}

// TestCheckRemoteSkipsCredentialsOfSwitchedOffSending: a purpose only
// sending resolves is diagnosed unless the channel's configuration
// switches send off (reported disabled), as a receive-only channel never
// presents it. Unavailable for another reason, send is still run, and its
// credential still needed; a missing one says so.
func TestCheckRemoteSkipsCredentialsOfSwitchedOffSending(t *testing.T) {
	reg := adapters.NewRegistry()
	a := adapterstest.New().Adapter()
	a.CredentialPurposes = []string{"webhook", "callback"}
	a.CredentialSending = []string{"callback"}
	if err := reg.Register(a); err != nil {
		t.Fatal(err)
	}
	w := newWorldWith(t, reg)
	w.createTyped("receive-only", adapterstest.Type, adapterstest.Config{Account: "a", Disabled: []model.Capability{model.CapabilitySend}})
	w.createTyped("sends", adapterstest.Type, adapterstest.Config{Account: "b"})
	for key, want := range map[string][]string{
		"receive-only": {"webhook"},
		"sends":        {"webhook", "callback"},
	} {
		d := &purposeDiag{ok: []string{"webhook"}}
		got, err := w.as(operator).Check(context.Background(), key, channels.CheckOptions{Remote: true, Credentials: d})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		c := got[0]
		if !slices.Equal(d.seen, want) || len(c.Remote.Credentials) != len(want) {
			t.Errorf("%s: diagnosed %v (%d reported), want %v", key, d.seen, len(c.Remote.Credentials), want)
		}
		f, found := findingFor(c, "credential:callback")
		if found != slices.Contains(want, "callback") {
			t.Errorf("%s: finding for callback = %v, want %v", key, found, slices.Contains(want, "callback"))
		}
		if found && (f.Severity != channels.SeverityError || !strings.Contains(f.Message, "needed for send")) {
			t.Errorf("%s: callback finding = %+v", key, f)
		}
	}
}

// TestCheckRemoteDiscordCredentialsPerReceiveMode: a discord.chat channel
// needs its bot token always and the application's public key only when
// it takes the Interactions Endpoint URL: under receive gateway the
// daemon never mounts the route, so no key is asked for. With receive
// empty both ways run, and the key is needed.
func TestCheckRemoteDiscordCredentialsPerReceiveMode(t *testing.T) {
	all, err := composition.Adapters(composition.Options{})
	if err != nil {
		t.Fatal(err)
	}
	reg := adapters.NewRegistry()
	for _, a := range all {
		if a.Factory.Describe().Type == discord.Type {
			if err := reg.Register(a); err != nil {
				t.Fatal(err)
			}
		}
	}
	w := newWorldWith(t, reg)
	api, key := discord.CredentialPurposeAPI, discord.CredentialPurposeInteractions
	for i, tc := range []struct {
		mode discord.ReceiveMode
		want []string
	}{
		{discord.ReceiveGateway, []string{api}},
		{discord.ReceiveInteractions, []string{api, key}},
		{"", []string{api, key}},
	} {
		name := "bot-" + string(tc.mode)
		w.createTyped(name, discord.Type, discord.Config{
			ApplicationID: "90000000000000000" + string(rune('1'+i)), BotUserID: "900000000000000009", Receive: tc.mode,
		})
		d := &purposeDiag{ok: []string{api}}
		got, err := w.as(operator).Check(context.Background(), name, channels.CheckOptions{Remote: true, Credentials: d})
		if err != nil {
			t.Fatalf("receive %q: %v", tc.mode, err)
		}
		if !slices.Equal(d.seen, tc.want) {
			t.Errorf("receive %q: diagnosed %v, want %v", tc.mode, d.seen, tc.want)
		}
		if _, found := findingFor(got[0], "credential:"+key); found != slices.Contains(tc.want, key) {
			t.Errorf("receive %q: missing public key reported = %v; findings %+v", tc.mode, found, got[0].Findings)
		}
	}
}

// TestCheckRemoteReportsReceiving: a remote check reports each way the
// channel receives as the running daemon runs it: the persistent receiver
// (a Discord gateway, a socket, a poll) and the webhook route. A way the
// channel's configuration switches off is "off" and the daemon is not
// asked. A receiver that failed is an error when the channel's inboxes
// need it, a waiting one a warning, a starting or ready one nothing; one
// the channel does not need is reported without a finding.
func TestCheckRemoteReportsReceiving(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.createTyped("both", adapterstest.Type, adapterstest.Config{Account: "a"})
	w.createTyped("gateway", adapterstest.Type, adapterstest.Config{Account: "b", Disabled: []model.Capability{model.CapabilityReceiveHTTP}})
	w.createInbox("both", "both-in", "dest-a")
	w.createInbox("gateway", "gateway-in", "dest-b")
	w.createTyped("idle", adapterstest.Type, adapterstest.Config{Account: "c"})
	s := w.as(operator)
	since := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	asked := map[string][]model.Capability{}
	state := map[model.Capability]channels.ReceivingState{
		model.CapabilityReceive:     {State: "ready", Since: since, Owner: "heo/default/receiver", Until: since.Add(time.Minute)},
		model.CapabilityReceiveHTTP: {State: "ready", Since: since, Path: "/webhooks/x"},
	}
	opts := func() channels.CheckOptions {
		return channels.CheckOptions{Remote: true, Receiving: func(_ context.Context, ch model.Channel, way model.Capability) channels.ReceivingState {
			asked[ch.Key] = append(asked[ch.Key], way)
			st := state[way]
			st.Way = way
			return st
		}}
	}
	byWay := func(c channels.ChannelCheck) map[model.Capability]channels.ReceivingState {
		out := map[model.Capability]channels.ReceivingState{}
		for _, r := range c.Remote.Receiving {
			out[r.Way] = r
		}
		return out
	}

	got, err := s.Check(ctx, "both", opts())
	if err != nil {
		t.Fatal(err)
	}
	if r := byWay(got[0]); len(r) != 2 || r[model.CapabilityReceive].State != "ready" || r[model.CapabilityReceive].Owner != "heo/default/receiver" ||
		r[model.CapabilityReceiveHTTP].Path != "/webhooks/x" {
		t.Errorf("receiving = %+v, want both ways ready, the receiver with its lease and the route with its path", got[0].Remote.Receiving)
	}
	for _, f := range got[0].Findings {
		if strings.HasPrefix(f.Scope, "receiving:") {
			t.Errorf("a ready receiver raised %+v", f)
		}
	}

	// The configuration switches the route off: reported off, not asked.
	got, err = s.Check(ctx, "gateway", opts())
	if err != nil {
		t.Fatal(err)
	}
	r := byWay(got[0])
	if r[model.CapabilityReceiveHTTP].State != "off" || !strings.Contains(r[model.CapabilityReceiveHTTP].Reason, "configuration") ||
		r[model.CapabilityReceive].State != "ready" {
		t.Errorf("receiving = %+v, want the route off by configuration and the receiver ready", got[0].Remote.Receiving)
	}
	if slices.Contains(asked["gateway"], model.CapabilityReceiveHTTP) {
		t.Error("the daemon was asked about a way the configuration switched off")
	}

	// Failed: an error where the inbox needs it; waiting: a warning.
	state[model.CapabilityReceive] = channels.ReceivingState{State: "failed", Reason: "disabled: a privileged intent is not granted (close 4014)"}
	got, _ = s.Check(ctx, "both", opts())
	if !hasFinding(got[0], channels.SeverityError, "receiving:receive", "receiver_failed") || got[0].OK {
		t.Errorf("a failed receiver = %+v, want an error finding", got[0].Findings)
	}
	if f, _ := findingFor(got[0], "receiving:receive"); !strings.Contains(f.Message, "close 4014") {
		t.Errorf("finding = %+v, want the receiver's reason", f)
	}
	state[model.CapabilityReceive] = channels.ReceivingState{State: "waiting", Reason: "rate_limited (retrying in 1h0m0s)"}
	got, _ = s.Check(ctx, "both", opts())
	if !hasFinding(got[0], channels.SeverityWarning, "receiving:receive", "receiver_waiting") || !got[0].OK {
		t.Errorf("a waiting receiver = %+v, want a warning and still OK", got[0].Findings)
	}
	state[model.CapabilityReceive] = channels.ReceivingState{State: "starting"}
	got, _ = s.Check(ctx, "both", opts())
	if _, found := findingFor(got[0], "receiving:receive"); found {
		t.Errorf("a starting receiver raised %+v", got[0].Findings)
	}

	// A channel with no inbox needs no receiver: reported, no finding.
	state[model.CapabilityReceive] = channels.ReceivingState{State: "failed", Reason: "x"}
	got, _ = s.Check(ctx, "idle", opts())
	if len(got[0].Remote.Receiving) != 2 {
		t.Errorf("receiving = %+v, want both ways reported", got[0].Remote.Receiving)
	}
	if _, found := findingFor(got[0], "receiving:receive"); found {
		t.Errorf("a receiver nothing needs raised %+v", got[0].Findings)
	}

	// Without a view of the daemon nothing is reported.
	got, _ = s.Check(ctx, "both", channels.CheckOptions{Remote: true})
	if got[0].Remote.Receiving == nil || len(got[0].Remote.Receiving) != 0 {
		t.Errorf("receiving without a daemon view = %#v, want an empty list", got[0].Remote.Receiving)
	}
}

// TestCheckRemoteReceivingNeedsOneWay: an inbox with destinations needs
// the channel to receive, by either way. A way unavailable while the
// other works (a polling mailbox with no push topic, a socket channel
// with the request URL switched off) raises no capability finding; only
// every declared way being unavailable does.
func TestCheckRemoteReceivingNeedsOneWay(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.createTyped("poll", adapterstest.Type, adapterstest.Config{Account: "a", NoWebhook: true})
	w.createTyped("socket", adapterstest.Type, adapterstest.Config{Account: "b", Disabled: []model.Capability{model.CapabilityReceiveHTTP}})
	w.createTyped("none", adapterstest.Type, adapterstest.Config{
		Account: "c", NoWebhook: true, Disabled: []model.Capability{model.CapabilityReceive},
	})
	w.createInbox("poll", "poll-in", "dest-a")
	w.createInbox("socket", "socket-in", "dest-b")
	w.createInbox("none", "none-in", "dest-c")
	s := w.as(operator)
	for _, key := range []string{"poll", "socket"} {
		got, err := s.Check(ctx, key, channels.CheckOptions{Remote: true})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		for _, f := range got[0].Findings {
			if strings.HasPrefix(f.Scope, "capability:receive") {
				t.Errorf("%s: a channel that receives one way raised %+v", key, f)
			}
		}
		if !got[0].OK {
			t.Errorf("%s: check not OK: %+v", key, got[0].Findings)
		}
	}
	got, err := s.Check(ctx, "none", channels.CheckOptions{Remote: true})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].OK || !hasFinding(got[0], channels.SeverityError, "capability:receive", string(model.Disabled)) {
		t.Errorf("a channel that cannot receive = %+v, want an error finding", got[0].Findings)
	}
}

// statusDiag reports every purpose with one status.
type statusDiag struct{ status credentials.Status }

func (d statusDiag) Check(_ context.Context, req credentials.Request) credentials.Diagnosis {
	return credentials.Diagnosis{Reference: req.Reference, Purpose: req.Purpose, Status: d.status, Message: string(d.status)}
}

// TestCheckRemoteProbesACredentialThatIsStored: an adapter whose host
// makes its credential out of the stored one is tried by a remote check,
// and what fails is an error finding scoped to the stored purpose, with the
// provider's own code and the probe's own message. The probe runs only
// when the stored credential is there (present, or rejected by the
// provider, which it explains): a missing one is the diagnosis's to say.
func TestCheckRemoteProbesACredentialThatIsStored(t *testing.T) {
	reg := adapters.NewRegistry()
	a := adapterstest.New().Adapter()
	a.CredentialPurposes = []string{"client_secret"}
	var probed []model.ChannelRef
	problem := &adapters.CredentialProblem{Purpose: "client_secret", Code: "invalid_client", Message: "the token endpoint refused it (invalid_client, AADSTS7000215)"}
	a.ProbeCredential = func(_ context.Context, ch model.Channel) *adapters.CredentialProblem {
		probed = append(probed, ch.Ref)
		return problem
	}
	if err := reg.Register(a); err != nil {
		t.Fatal(err)
	}
	w := newWorldWith(t, reg)
	w.createTyped("bot", adapterstest.Type, adapterstest.Config{Account: "a"})

	run := func(d credentials.Status) channels.ChannelCheck {
		t.Helper()
		got, err := w.as(operator).Check(context.Background(), "bot", channels.CheckOptions{Remote: true, Credentials: statusDiag{status: d}})
		if err != nil {
			t.Fatal(err)
		}
		return got[0]
	}
	c := run(credentials.StatusOK)
	f, found := findingFor(c, "credential:client_secret")
	if !found || f.Severity != channels.SeverityError || f.Code != "invalid_client" || f.Message != problem.Message || c.OK {
		t.Errorf("a stored credential the probe refuses: finding %+v (found %v), ok=%v", f, found, c.OK)
	}
	if len(probed) != 1 {
		t.Fatalf("probed %d times, want once", len(probed))
	}

	probed = nil
	c = run(credentials.StatusRejected)
	if len(probed) != 1 {
		t.Errorf("a rejected credential was probed %d times, want once: the probe says why", len(probed))
	}
	var scoped []channels.Finding
	for _, f := range c.Findings {
		if f.Scope == "credential:client_secret" {
			scoped = append(scoped, f)
		}
	}
	if len(scoped) != 1 || scoped[0].Code != "invalid_client" {
		t.Errorf("rejected = %+v, want the probe's finding alone: it says why", c.Findings)
	}
	// A rejection the host can no longer reproduce is not an error.
	saved := problem
	problem = nil
	if c := run(credentials.StatusRejected); !c.OK {
		t.Errorf("rejected, but the probe passes: %+v", c.Findings)
	}
	problem = saved

	probed = nil
	c = run(credentials.StatusMissing)
	if len(probed) != 0 {
		t.Error("a missing credential was probed")
	}
	if f, found := findingFor(c, "credential:client_secret"); !found || f.Code != string(credentials.StatusMissing) {
		t.Errorf("missing = %+v", c.Findings)
	}

	problem = nil
	probed = nil
	if c := run(credentials.StatusOK); !c.OK || len(probed) != 1 {
		t.Errorf("a probe that passes: ok=%v after %d probes, findings %+v", c.OK, len(probed), c.Findings)
	}
}

// refuser refuses the grants of the references it names.
type refuser struct {
	refs map[string]string
	seen []string
}

func (r *refuser) RefuseGrant(_ context.Context, req credentials.Request) *credentials.GrantRefusal {
	r.seen = append(r.seen, req.Reference+"/"+req.Purpose)
	if msg, ok := r.refs[req.Reference]; ok {
		return &credentials.GrantRefusal{Grant: "refresh_token", Message: msg}
	}
	return nil
}

// TestCheckRefusesAGrantTheBackendCannotServe: the local check, the
// one `config validate` runs, reports an error finding for a stored
// grant the backend refuses, once for purposes sharing one entry, and
// reaches no provider.
func TestCheckRefusesAGrantTheBackendCannotServe(t *testing.T) {
	reg := adapters.NewRegistry()
	a := adapterstest.New().Adapter()
	a.CredentialPurposes = []string{"api", "signing"}
	if err := reg.Register(a); err != nil {
		t.Fatal(err)
	}
	w := newWorldWith(t, reg)
	w.createTyped("oauth", adapterstest.Type, adapterstest.Config{Account: "a"})
	w.createTyped("plain", adapterstest.Type, adapterstest.Config{Account: "b"})
	r := &refuser{refs: map[string]string{"cred-oauth": "needs a writable credential backend: keyring, file, openbao"}}
	got, err := w.as(operator).Check(context.Background(), "", channels.CheckOptions{Grants: r})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		var n int
		for _, f := range c.Findings {
			if f.Code == "grant_not_writable" {
				n++
				if f.Severity != channels.SeverityError || f.Scope != "credential:api" || !strings.Contains(f.Message, "writable") {
					t.Errorf("finding = %+v", f)
				}
			}
		}
		want := 0
		if c.Key == "oauth" {
			want = 1
		}
		if n != want || c.OK != (want == 0 && !slices.ContainsFunc(c.Findings, func(f channels.Finding) bool { return f.Severity == channels.SeverityError })) {
			t.Errorf("%s: %d grant findings (want %d), ok=%v", c.Key, n, want, c.OK)
		}
		if c.Remote != nil {
			t.Errorf("%s: a local check probed remotely", c.Key)
		}
	}
	// Without a refuser nothing is read.
	r.seen = nil
	if _, err := w.as(operator).Check(context.Background(), "", channels.CheckOptions{}); err != nil || len(r.seen) != 0 {
		t.Errorf("no refuser: %v, %v", err, r.seen)
	}
}
