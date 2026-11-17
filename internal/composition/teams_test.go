package composition_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/composition"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/chat/teams"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// entraStandIn is a token endpoint of our own: it issues a token to the
// one app and secret it is told, per tenant path.
type entraStandIn struct {
	srv *httptest.Server

	mu     sync.Mutex
	secret string
	paths  []string
	apps   []string
}

func newEntraStandIn(t *testing.T, secret string) *entraStandIn {
	t.Helper()
	e := &entraStandIn{secret: secret}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		_ = r.ParseForm()
		e.mu.Lock()
		e.paths = append(e.paths, r.URL.Path)
		e.apps = append(e.apps, r.PostForm.Get("client_id"))
		want := e.secret
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("client_secret") != want {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "invalid_client", "error_description": "AADSTS7000215: Invalid client secret provided. (" + r.PostForm.Get("client_secret") + ")",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer", "expires_in": 3600, "access_token": "minted-for" + r.URL.Path})
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *entraStandIn) requests() ([]string, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.paths...), append([]string(nil), e.apps...)
}

// secrets is a credentials port holding client secrets by reference, which
// records the purposes it was asked for.
type secrets struct {
	mu      sync.Mutex
	byRef   map[string]string
	purpose []string
	fresh   int
}

func (s *secrets) Resolve(_ context.Context, req ports.CredentialRequest) (ports.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purpose = append(s.purpose, req.Purpose)
	if req.Fresh {
		s.fresh++
	}
	v, ok := s.byRef[req.Reference]
	if !ok || req.Purpose != composition.PurposeTeamsClientSecret {
		return nil, model.NewError(model.NotFound, "no credential stored")
	}
	return ports.Secret{Bytes: []byte(v)}, nil
}

func teamsChannel(ref, id, tenant, app string) model.Channel {
	cfg := teams.Config{AppID: app, TenantID: tenant, ServiceURL: "https://smba.trafficmanager.net/teams/"}
	return model.Channel{
		Ref: model.ChannelRef{Tenant: "default", ID: model.ID(id)}, Type: teams.Type, Key: id,
		CredentialRef: ref, Configuration: cfg.Extension(),
	}
}

func teamsAdapter(t *testing.T, authority string) adapters.Adapter {
	t.Helper()
	r := registry(t, settings(composition.KeyTeamsAuthority, authority))
	a, ok := r.Lookup(teams.Type)
	if !ok {
		t.Fatal("microsoft.teams is not registered")
	}
	return a
}

// TestTeamsSettings: where the client secret may be sent is a host
// setting, checked offline: https, or http on a loopback host; never a
// channel's.
func TestTeamsSettings(t *testing.T) {
	cfg, probs := composition.ReadTeams(settings())
	if len(probs) != 0 || cfg.Authority != "https://login.microsoftonline.com" {
		t.Errorf("default = %+v %v", cfg, probs)
	}
	for in, ok := range map[string]bool{
		"https://login.microsoftonline.us": true, "http://127.0.0.1:9": true,
		"http://login.microsoftonline.com": false, "https://u:p@login.microsoftonline.com": false, "login.microsoftonline.com": false,
	} {
		_, probs := composition.ReadTeams(settings(composition.KeyTeamsAuthority, in))
		if ok != (len(probs) == 0) {
			t.Errorf("%s = %v, want ok=%v", in, probs, ok)
		}
		var checked bool
		for _, p := range composition.Check(t.Context(), settings(composition.KeyTeamsAuthority, in), nil) {
			checked = checked || p.Key == composition.KeyTeamsAuthority
		}
		if checked == ok {
			t.Errorf("config validate for %s reports the key: %v, want %v", in, checked, !ok)
		}
	}
}

// Each channel mints for its own bot and tenant; the Connector purpose is
// the minted token and every other purpose is the backend's; the minter
// outlives the instance; the probe says what Entra said and never the
// secret.
func TestTeamsFactoryMintsPerChannel(t *testing.T) {
	const secretA, secretB = "secret-of-a~1", "secret-of-b~2"
	const tenantA, appA = "1111aaaa-1111-4111-8111-11111111bbbb", "2222cccc-2222-4222-8222-22222222dddd"
	const tenantB, appB = "3333eeee-3333-4333-8333-33333333ffff", "4444aaaa-4444-4444-8444-44444444bbbb"
	e := newEntraStandIn(t, secretA)
	a := teamsAdapter(t, e.srv.URL)
	creds := &secrets{byRef: map[string]string{"a": secretA, "b": "wrong-" + secretB}}
	deps := adapter.Dependencies{Credentials: creds, HTTP: e.srv.Client()}

	chA, chB := teamsChannel("a", "00000000-0000-4000-8000-00000000000a", tenantA, appA), teamsChannel("b", "00000000-0000-4000-8000-00000000000b", tenantB, appB)
	for _, ch := range []model.Channel{chA, chB} {
		inst, err := a.Factory.Open(t.Context(), ch, deps)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = inst.Close(context.Background()) })
	}
	if p := a.ProbeCredential(t.Context(), chA); p != nil {
		t.Fatalf("probe of the bot whose secret is right = %+v", p)
	}
	p := a.ProbeCredential(t.Context(), chB)
	if p == nil || p.Purpose != composition.PurposeTeamsClientSecret || p.Code != "invalid_client" ||
		!strings.Contains(p.Message, "AADSTS7000215") || !strings.Contains(p.Message, composition.PurposeTeamsClientSecret) {
		t.Fatalf("probe of the bot whose secret is wrong = %+v, want invalid_client, AADSTS7000215, the purpose to fix", p)
	}
	if strings.Contains(p.Message, "secret-of-b") || strings.Contains(p.Message, "wrong-") {
		t.Errorf("the probe's message holds the secret: %q", p.Message)
	}
	paths, apps := e.requests()
	if len(paths) != 2 || paths[0] != "/"+tenantA+"/oauth2/v2.0/token" || paths[1] != "/"+tenantB+"/oauth2/v2.0/token" || apps[0] != appA || apps[1] != appB {
		t.Errorf("requests %v %v, want each bot minted at its own tenant", paths, apps)
	}
	if creds.fresh != 1 {
		t.Errorf("the backend was told of a rejected secret %d times, want once (the bot whose secret Entra refused)", creds.fresh)
	}
	for _, purpose := range creds.purpose {
		if purpose != composition.PurposeTeamsClientSecret && purpose != composition.PurposeTeamsClientCertificate {
			t.Errorf("the backend was asked for %q, want only the bot's client secret or certificate", purpose)
		}
	}
	if p := a.ProbeCredential(t.Context(), teamsChannel("c", "00000000-0000-4000-8000-00000000000c", tenantA, appA)); p != nil {
		t.Errorf("a channel never opened was probed: %+v", p)
	}

	// The secret is read at every mint: the endpoint now accepts what the
	// backend holds for the channel, and the probe, on a channel opened
	// again, passes at once (it ignores the backoff the failure started).
	e.mu.Lock()
	e.secret = "wrong-" + secretB
	e.mu.Unlock()
	if _, err := a.Factory.Open(t.Context(), chB, deps); err != nil {
		t.Fatal(err)
	}
	if p := a.ProbeCredential(t.Context(), chB); p != nil {
		t.Errorf("after the endpoint accepted the stored secret, probe = %+v", p)
	}
}

// TestTeamsFactoryOpensWithoutAnHTTPClient: Open with nothing to mint with
// is uma's: the instance opens and its calls fail as unavailable.
func TestTeamsFactoryOpensWithoutAnHTTPClient(t *testing.T) {
	a := teamsAdapter(t, "")
	inst, err := a.Factory.Open(t.Context(), teamsChannel("a", "00000000-0000-4000-8000-00000000000a", "1111aaaa-1111-4111-8111-11111111bbbb", "2222cccc-2222-4222-8222-22222222dddd"), adapter.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	_ = inst.Close(context.Background())
}

// TestTeamsFactoryRefusesAnAuthorityThatLeaksTheSecret: a setting naming a
// plain http host that is not loopback fails the open, so the secret is
// never sent there.
func TestTeamsFactoryRefusesAnAuthorityThatLeaksTheSecret(t *testing.T) {
	a := teamsAdapter(t, "http://login.example.com")
	_, err := a.Factory.Open(t.Context(), teamsChannel("a", "00000000-0000-4000-8000-00000000000a", "1111aaaa-1111-4111-8111-11111111bbbb", "2222cccc-2222-4222-8222-22222222dddd"),
		adapter.Dependencies{Credentials: &secrets{}, HTTP: http.DefaultClient})
	if err == nil || !strings.Contains(err.Error(), composition.KeyTeamsAuthority) {
		t.Errorf("Open = %v, want an error naming %s", err, composition.KeyTeamsAuthority)
	}
}
