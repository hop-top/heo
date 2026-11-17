package composition_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/profile"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/meta"
	metawhatsapp "hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/model"
)

// nopSink is a webhook sink no test request reaches: the requests below
// carry no message.
type nopSink struct{}

func (nopSink) Accept(context.Context, model.Ingress) (model.IngressReceipt, error) {
	return model.IngressReceipt{}, model.NewError(model.Permanent, "unexpected ingress")
}

func (nopSink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, model.NewError(model.Permanent, "unexpected interaction")
}

// TestMultiSecretAdapterGetsOneSecretPerPurpose: meta.whatsapp, opened
// as heo registers it on heo's credential backend, answers Meta's
// subscription handshake with the verify token's own entry and checks
// signatures with the app secret's own entry: two values behind one
// credential reference. Only heo's side of the webhook runs; no
// provider is called.
func TestMultiSecretAdapterGetsOneSecretPerPurpose(t *testing.T) {
	ctx := t.Context()
	b, err := credentials.Open(ctx, credentials.Options{
		Profile: profile.Default(), Tenant: "default",
		Config: credentials.Config{Backend: credentials.BackendFile, KeyringService: "heo", FileDir: t.TempDir(), OpenbaoMount: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	const verifyToken, appSecret = "verify-token-value", "app-secret-value"
	for purpose, value := range map[string]string{
		metawhatsapp.PurposeVerifyToken: verifyToken,
		metawhatsapp.PurposeAppSecret:   appSecret,
	} {
		keys, err := b.Keys("wa", purpose)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": value})
		if err := b.SurfaceTokens().Set(ctx, keys[0], data); err != nil {
			t.Fatal(err)
		}
	}

	a, ok := registry(t, settings()).Lookup(model.TypeMetaWhatsapp)
	if !ok {
		t.Fatal("meta.whatsapp is not registered")
	}
	ch := model.Channel{
		Ref: model.ChannelRef{Tenant: "default", ID: "01890000-0000-7000-8000-000000000001"}, Type: model.TypeMetaWhatsapp, CredentialRef: "wa",
		Configuration: metawhatsapp.Config{PhoneNumberID: "100000000000001"}.Extension(),
	}
	inst, err := a.Factory.Open(ctx, ch, adapter.Dependencies{Credentials: b.Channels()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	h := inst.(adapter.HTTPReceiver).Handler(nopSink{})

	handshake := func(token string) (int, string) {
		q := url.Values{meta.ParamMode: {"subscribe"}, meta.ParamVerifyToken: {token}, meta.ParamChallenge: {"4711"}}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?"+q.Encode(), nil))
		body, _ := io.ReadAll(w.Result().Body)
		return w.Code, string(body)
	}
	if code, body := handshake(verifyToken); code != http.StatusOK || body != "4711" {
		t.Errorf("handshake with the verify token = %d %q, want 200 with the challenge", code, body)
	}
	if code, _ := handshake(appSecret); code != http.StatusForbidden {
		t.Errorf("handshake with the app secret = %d, want 403", code)
	}

	notify := func(key string) int {
		body := `{"object":"page","entry":[]}`
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(meta.SignatureHeader, meta.Sign([]byte(key), []byte(body)))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := notify(appSecret); code != http.StatusOK {
		t.Errorf("notification signed with the app secret = %d, want 200", code)
	}
	if code := notify(verifyToken); code != http.StatusUnauthorized {
		t.Errorf("notification signed with the verify token = %d, want 401", code)
	}
}
