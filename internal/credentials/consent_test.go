package credentials

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hop.top/heo/internal/profile"
	"hop.top/kit/go/storage/secret/oauth"
	"hop.top/uma/model"
)

const (
	gmailGrantKey   = "uma/default/gmail"
	gmailRefreshKey = "uma/default/gmail.refresh_token"
)

// consentGrant is the grant a first consent returns for the reference
// gmail, naming a refresh token stored beside it.
func consentGrant(i *idp) oauth.Grant {
	return oauth.Grant{Kind: oauth.RefreshToken, TokenURL: i.url(), ClientID: "heo-client", Scopes: []string{"mail.read"},
		SecretRef: gmailRefreshKey}
}

func storeGrant(t *testing.T, b *Backend, i *idp, mod func(*GrantStore)) (GrantStored, error) {
	t.Helper()
	in := GrantStore{Reference: "gmail", Grant: consentGrant(i), RefreshSecret: secretEnv(i.rt)}
	if mod != nil {
		mod(&in)
	}
	return b.StoreRefreshGrant(context.Background(), in)
}

// TestStoreRefreshGrantKeepsTheTokensAndMintsFromThem: a consent's grant
// and refresh token are stored under the reference and the key the grant
// names, and the backend then mints access tokens from them with no further
// help: the refresh token reaches the token endpoint as it was returned.
func TestStoreRefreshGrantKeepsTheTokensAndMintsFromThem(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}
	b := oauthBackend(t, clk)
	got, err := storeGrant(t, b, i, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.GrantKey != gmailGrantKey || got.RefreshKey != gmailRefreshKey || got.Replaced != "" || len(got.Shadowed) != 0 {
		t.Fatalf("stored = %+v", got)
	}
	e, err := b.Entry(ctx, "gmail")
	if err != nil || !e.Exists || e.Key != gmailGrantKey || e.Kind != "refresh_token" || e.Grant == nil ||
		e.Grant.ClientID != "heo-client" || e.Grant.SecretRef != gmailRefreshKey {
		t.Fatalf("entry = %+v, %v", e, err)
	}
	c, err := b.Resolve(ctx, oreq("gmail", false))
	if o := token(t, c, err); o.AccessToken != seed+"-AT-1" {
		t.Errorf("access token = %q", o.AccessToken)
	}
	if f := i.form(0); f.Get("grant_type") != "refresh_token" || f.Get("refresh_token") != i.rt {
		t.Errorf("exchange = %v, want the refresh token that was stored", f)
	}
}

// TestStoreRefreshGrantReplacesAnEarlierConsent: a second consent replaces
// the refresh_token grant and its token without --replace, drops what the
// provider rejected before, and the next resolve uses the new token.
func TestStoreRefreshGrantReplacesAnEarlierConsent(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}
	b := oauthBackend(t, clk)
	if _, err := storeGrant(t, b, i, nil); err != nil {
		t.Fatal(err)
	}
	i.rt = seed + "-RT-second"
	got, err := storeGrant(t, b, i, func(in *GrantStore) { in.RefreshSecret = secretEnv(i.rt) })
	if err != nil || got.Replaced != "refresh_token" {
		t.Fatalf("second consent = %+v, %v", got, err)
	}
	c, err := b.Resolve(ctx, oreq("gmail", false))
	token(t, c, err)
	if f := i.form(i.calls() - 1); f.Get("refresh_token") != seed+"-RT-second" {
		t.Errorf("exchange = %v, want the second consent's token", f)
	}
}

// TestStoreRefreshGrantNeverDestroysACredentialUnseen: a reference that
// holds a password, a static token or another grant kind is refused with
// its kind named and left as it was, until the caller says it may be
// lost; the refresh token is not written either.
func TestStoreRefreshGrantNeverDestroysACredentialUnseen(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}
	for name, existing := range map[string][]byte{
		"password":           envelope(t, "hunter2-seed"),
		"secret":             secretEnv(seed + "-static"),
		"client_credentials": grantEnv(t, oauth.Grant{Kind: oauth.ClientCredentials, TokenURL: i.url(), ClientID: "c", SecretRef: "uma/default/cs"}), //nolint:gosec // fixture grant, not a credential
	} {
		t.Run(name, func(t *testing.T) {
			b := oauthBackend(t, clk)
			put(t, b, "gmail", existing)
			_, err := storeGrant(t, b, i, nil)
			if !model.IsCode(err, model.Conflict) || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "--replace") {
				t.Fatalf("store over a %s: %v", name, err)
			}
			noSeed(t, "refusal", err.Error())
			if sec, err := b.SurfaceTokens().Get(ctx, gmailGrantKey); err != nil || string(sec.Value) != string(existing) {
				t.Errorf("the entry changed: %s, %v", sec.Value, err)
			}
			if ok, _ := b.SurfaceTokens().Exists(ctx, gmailRefreshKey); ok {
				t.Error("the refresh token was written though the grant was refused")
			}
			got, err := storeGrant(t, b, i, func(in *GrantStore) { in.Replace = true })
			if err != nil || got.Replaced != name {
				t.Errorf("with Replace: %+v, %v", got, err)
			}
			if e, _ := b.Entry(ctx, "gmail"); e.Grant == nil || e.Grant.Kind != oauth.RefreshToken {
				t.Errorf("entry after replace = %+v", e)
			}
		})
	}
}

// TestStoreRefreshGrantNamesWhatShadowsIt: a purpose with an entry of its
// own keeps it and never uses the grant: the store says which.
func TestStoreRefreshGrantNamesWhatShadowsIt(t *testing.T) {
	i, clk := newIDP(t), &clock{}
	b := oauthBackend(t, clk)
	for _, purpose := range []string{"send", "api"} {
		keys, err := b.Keys("gmail", purpose)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SurfaceTokens().Set(context.Background(), keys[0], secretEnv("x")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := storeGrant(t, b, i, func(in *GrantStore) { in.Purposes = []string{"send", "receive", "api"} })
	if err != nil || strings.Join(got.Shadowed, ",") != "api,send" {
		t.Errorf("shadowed = %v, %v", got.Shadowed, err)
	}
}

// TestStoreRefreshGrantRefusals: a backend that cannot be written, a grant
// of another kind, a grant that names its own entry or one outside the
// profile's references, and no token to keep are refused before anything is
// written.
func TestStoreRefreshGrantRefusals(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}

	ro := readOnlyBackend(t, clk)
	_, err := storeGrant(t, ro, i, nil)
	if !model.IsCode(err, model.Unsupported) {
		t.Errorf("read-only backend: %v", err)
	}
	for _, want := range []string{"writable", "keyring", "file", "openbao"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("read-only backend: %v lacks %q", err, want)
		}
	}
	if err := ro.CanKeepGrant(); !model.IsCode(err, model.Unsupported) {
		t.Errorf("CanKeepGrant on a read-only backend = %v", err)
	}
	apsRef := profile.Ref{Scheme: profile.ApsScheme, ID: "noor"}
	apsProfile(t, "noor", "UNRELATED='x'\n")
	aps, err := Open(ctx, Options{Profile: apsRef, Tenant: model.TenantID(apsRef.Dir()), Config: fileConfig(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aps.StoreRefreshGrant(ctx, GrantStore{Reference: "GMAIL", Grant: consentGrant(i), RefreshSecret: secretEnv("x")}); !model.IsCode(err, model.Unsupported) ||
		!strings.Contains(err.Error(), "aps secrets are read-only") {
		t.Errorf("aps profile: %v", err)
	}

	b := oauthBackend(t, clk)
	for name, mod := range map[string]func(*GrantStore){
		"another kind": func(in *GrantStore) {
			in.Grant = oauth.Grant{Kind: oauth.ClientCredentials, TokenURL: i.url(), ClientID: "c", SecretRef: gmailRefreshKey}
		},
		"invalid grant":  func(in *GrantStore) { in.Grant.ClientID = "" },
		"no token":       func(in *GrantStore) { in.RefreshSecret = nil },
		"its own entry":  func(in *GrantStore) { in.Grant.SecretRef = gmailGrantKey },
		"client secret":  func(in *GrantStore) { in.Grant.ClientSecretRef = gmailGrantKey },
		"foreign tenant": func(in *GrantStore) { in.Grant.SecretRef = "uma/other/gmail.refresh_token" },
		"outside":        func(in *GrantStore) { in.Grant.SecretRef = "heo/default/surface-token" },
		"bad reference":  func(in *GrantStore) { in.Reference = "../x" },
	} {
		if _, err := storeGrant(t, b, i, mod); err == nil || !(model.IsCode(err, model.Validation)) {
			t.Errorf("%s: %v, want a validation error", name, err)
		}
	}
	if ks, _ := b.SurfaceTokens().List(ctx, ""); len(ks) != 0 {
		t.Errorf("a refused store wrote %v", ks)
	}
}

// TestSecretAt: the secret a grant names is read from the profile's own
// credential references only, as the value of a secret envelope, and
// nothing else of the store is readable through it.
func TestSecretAt(t *testing.T) {
	ctx := context.Background()
	b := oauthBackend(t, &clock{})
	put(t, b, "client", secretEnv("the-client-secret"))
	put(t, b, "pw", envelope(t, "pw-seed"))
	if err := b.SurfaceTokens().Set(ctx, "heo/default/other", secretEnv("outside")); err != nil {
		t.Fatal(err)
	}
	got, err := b.SecretAt(ctx, "uma/default/client")
	if err != nil || string(got) != "the-client-secret" {
		t.Fatalf("SecretAt = %q, %v", got, err)
	}
	for key, code := range map[string]model.ErrorCode{
		"uma/default/missing": model.NotFound, "uma/default/pw": model.Validation,
		"heo/default/other": model.Validation, "uma/other/client": model.Validation,
	} {
		_, err := b.SecretAt(ctx, key)
		if !model.IsCode(err, code) {
			t.Errorf("SecretAt(%q) = %v, want %s", key, err, code)
		}
		noSeed(t, "SecretAt error", err.Error())
	}
}

// TestEntryNeverReturnsTheValue: Entry reports the kind and, for a grant,
// the grant, which names secrets and holds none; a missing reference and
// an unreadable value are told apart.
func TestEntryNeverReturnsTheValue(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}
	b := oauthBackend(t, clk)
	if e, err := b.Entry(ctx, "gmail"); err != nil || e.Exists || e.Key != gmailGrantKey {
		t.Errorf("missing = %+v, %v", e, err)
	}
	put(t, b, "pw", envelope(t, "pw-seed"))
	put(t, b, "junk", []byte("not json"))
	seedGrant(t, b, i, oauth.RefreshToken, "granted")
	for ref, want := range map[string]string{"pw": "password", "junk": "", "granted": "refresh_token"} {
		e, err := b.Entry(ctx, ref)
		if err != nil || !e.Exists || e.Kind != want {
			t.Errorf("entry %s = %+v, %v, want kind %q", ref, e, err, want)
		}
		if ref == "granted" && (e.Grant == nil || e.Grant.ClientSecretRef != "uma/default/cs") {
			t.Errorf("grant of %s = %+v", ref, e.Grant)
		}
		if ref != "granted" && e.Grant != nil {
			t.Errorf("%s reports a grant: %+v", ref, e.Grant)
		}
		b, _ := json.Marshal(e)
		noSeed(t, "entry", string(b))
	}
	if _, err := b.Entry(ctx, "../x"); !model.IsCode(err, model.Validation) {
		t.Errorf("bad reference: %v", err)
	}
}
