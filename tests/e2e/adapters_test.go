package e2e

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/composition"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/chat/discord"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/adapter/chat/teams"
	"hop.top/uma/adapter/chat/telegram"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/adapter/mail/microsoft"
	"hop.top/uma/adapter/mail/smtp"
	metawhatsapp "hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/adapter/messaging/twilio/sms"
	twiliowhatsapp "hop.top/uma/adapter/messaging/twilio/whatsapp"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	umapg "hop.top/uma/store/postgres"
)

// configure configures chans in tenant's store through the release
// build's adapters, as a channel command would, and returns their IDs
// by key.
func (db *testDB) configure(tenant model.TenantID, chans ...model.Channel) map[string]model.ID {
	db.t.Helper()
	ctx := db.t.Context()
	c := db.operator(tenant)
	out := map[string]model.ID{}
	for _, ch := range chans {
		ch.Ref = model.ChannelRef{Tenant: tenant}
		acc, err := c.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{
			Command: model.Command{Call: model.Call{Correlation: "e2e"}, Operation: model.OperationID("channel-" + ch.Key)},
			Channel: ch,
		})
		if err != nil {
			db.t.Fatalf("configure channel %s: %v", ch.Key, err)
		}
		ref, err := acc.Operation.Result.AsChannel()
		if err != nil {
			db.t.Fatal(err)
		}
		out[ch.Key] = ref.ID
	}
	return out
}

// operator is a uma client on db for tenant through the release build's
// adapters, acting as the test's own uid — the daemon's installation
// owner — as an operator's tool beside the daemon would act on the
// shared store. It runs no workers: the daemon delivers what it queues,
// rechecking each operation under its own policy first.
func (db *testDB) operator(tenant model.TenantID) uma.Client {
	db.t.Helper()
	return db.as(tenant, model.ActorID(me()))
}

// as is a uma client on db for tenant acting as actor. Its own engine
// admits everything, so what it queues reaches the daemon's workers
// whatever the daemon's policy says: the daemon's recheck decides.
func (db *testDB) as(tenant model.TenantID, actor model.ActorID) uma.Client {
	db.t.Helper()
	st, err := umapg.Open(db.t.Context(), db.pool(), umapg.Options{})
	if err != nil {
		db.t.Fatal(err)
	}
	list, err := composition.Adapters(composition.Options{})
	if err != nil {
		db.t.Fatal(err)
	}
	factories := make([]adapter.Factory, 0, len(list))
	var extensions []adapter.ExtensionDefinition
	for _, a := range list {
		factories = append(factories, a.Factory)
		extensions = append(extensions, a.Extensions...)
	}
	e, err := uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: factories, Extensions: extensions, Clock: clock{}, IDs: ids{}, Blobs: db.blobs,
	})
	if err != nil {
		db.t.Fatal(err)
	}
	db.t.Cleanup(func() { _ = e.Close(context.Background()) })
	c, err := e.For(model.Principal{Tenant: tenant, Actor: actor})
	if err != nil {
		db.t.Fatal(err)
	}
	return c
}

func extension(t *testing.T, schema string, v any) model.Extension {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return model.Extension{Name: schema, Version: 1, Data: data}
}

// TestConfigValidateAdapters: config validate checks the adapters' host
// settings offline: the smtp.mail listener and the widget's retention.
func TestConfigValidateAdapters(t *testing.T) {
	s := newSandbox(t)
	args := []string{"config", "validate", "--format", "json", "-c", composition.KeySMTPAddr + "=0.0.0.0:25"}
	r := s.run(nil, args...)
	s.want(r, 2, args...)
	if !strings.Contains(r.stdout, `"key": "`+composition.KeySMTPAddr+`"`) {
		t.Errorf("no issue for a remote smtp listener without opt-in:\n%s", r.stdout)
	}
	args = []string{"config", "validate", "--format", "json",
		"-c", composition.KeySMTPAddr + "=0.0.0.0:25", "-c", composition.KeySMTPAllowRemote + "=true"}
	r = s.run(nil, args...)
	s.want(r, 2, args...)
	if !strings.Contains(r.stdout, `"key": "`+composition.KeySMTPAllowRemote+`"`) {
		t.Errorf("no issue for a remote smtp listener without STARTTLS:\n%s", r.stdout)
	}
	args = []string{"config", "validate", "--format", "json", "-c", composition.KeyWidgetRetain + "=0"}
	r = s.run(nil, args...)
	s.want(r, 2, args...)
	if !strings.Contains(r.stdout, `"key": "`+composition.KeyWidgetRetain+`"`) {
		t.Errorf("no issue for a widget retaining nothing:\n%s", r.stdout)
	}
	args = []string{"config", "validate",
		"-c", composition.KeySMTPAddr + "=0.0.0.0:25", "-c", composition.KeySMTPAllowRemote + "=true",
		"-c", composition.KeySMTPAllowRemoteWithoutTLS + "=true",
		"-c", "services.webhook.enabled=true", "-c", "services.webhook.public_base_url=https://hooks.example.com/heo"}
	r = s.run(nil, args...)
	s.want(r, 0, args...)
}

// TestConfigValidateSMTPTLS: config validate reads the listener
// certificate file and resolves its key in the profile's credential
// backend; a remote listener with a matching pair is valid, a key that
// does not match the certificate is not.
func TestConfigValidateSMTPTLS(t *testing.T) {
	s := newSandbox(t)
	certPEM, keyPEM := selfSigned(t)
	_, otherKey := selfSigned(t)
	certFile := s.write("mx.pem", string(certPEM))
	store := func(key []byte) {
		env, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": string(key)})
		if err != nil {
			t.Fatal(err)
		}
		s.write("secrets/uma/default/mx-key", string(env))
	}
	base := []string{"config", "validate", "--format", "json",
		"-c", "credentials.backend=file", "-c", "credentials.file.dir=" + s.path("secrets"),
		"-c", composition.KeySMTPAddr + "=0.0.0.0:25", "-c", composition.KeySMTPAllowRemote + "=true",
		"-c", composition.KeySMTPTLSCertFile + "=" + certFile, "-c", composition.KeySMTPTLSKeyRef + "=mx-key"}

	r := s.run(nil, base...)
	s.want(r, 2, base...)
	if !strings.Contains(r.stdout, `"key": "`+composition.KeySMTPTLSKeyRef+`"`) || !strings.Contains(r.stdout, "no credential stored") {
		t.Errorf("no issue for an unstored listener key:\n%s", r.stdout)
	}
	store(keyPEM)
	r = s.run(nil, base...)
	s.want(r, 0, base...)
	store(otherKey)
	r = s.run(nil, base...)
	s.want(r, 2, base...)
	if !strings.Contains(r.stdout, "does not match the certificate") || strings.Contains(r.stdout, "PRIVATE KEY") {
		t.Errorf("mismatched pair not reported, or reported with material:\n%s", r.stdout)
	}
}

// selfSigned returns a PEM certificate for mx.example.com and its PEM
// private key.
func selfSigned(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mx.example.com"}, DNSNames: []string{"mx.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})
}

// TestRuntimeDirectMessageChannels: the release binary starts with one
// channel per direct-message adapter and no credential stored: every
// service reports ready, each webhook channel answers at its route,
// the smtp.mail MX greets on its listener, and the receivers that need
// a credential report their channel by scope.
func TestRuntimeDirectMessageChannels(t *testing.T) {
	db := newDB(t).migrate()
	hookURL := "https://hooks.example.com/heo/webhooks/"
	ids := db.configure("default",
		model.Channel{Key: "widget", Type: widget.Type, Configuration: widget.Configuration("site-1"), CredentialRef: "widget-key"},
		model.Channel{Key: "mx", Type: model.TypeSmtpMail, CredentialRef: "relay",
			Configuration: extension(t, smtp.ConfigSchema, smtp.Config{Hostname: "mx.example.com", From: "support@example.com",
				Recipients: []smtp.Recipient{{Address: "support@example.com", Destination: "support"}},
				Relay:      smtp.Relay{Host: "relay.example.com", TLSPort: 465}})},
		model.Channel{Key: "mailbox", Type: imap.Type, CredentialRef: "mailbox",
			Configuration: extension(t, imap.ConfigSchema, imap.Config{Address: "support@example.com",
				IMAP: imap.Server{Host: "imap.example.com", TLSPort: 993}, Submission: imap.Server{Host: "smtp.example.com", TLSPort: 465}})},
		model.Channel{Key: "gmail", Type: google.Type, CredentialRef: "gmail",
			Configuration: extension(t, google.ConfigSchema, google.Config{User: "support@example.com",
				Topic: "projects/heo-test/topics/gmail-push", PushAudience: hookURL + "gmail", PushServiceAccount: "push@heo-test.iam.gserviceaccount.com"})},
		model.Channel{Key: "outlook", Type: microsoft.Type, CredentialRef: "graph",
			Configuration: extension(t, microsoft.ConfigSchema, microsoft.Config{Mailbox: "support@example.com", NotificationURL: hookURL + "outlook"})},
		model.Channel{Key: "sms", Type: model.TypeTwilioSms, CredentialRef: "twilio",
			Configuration: sms.Config{AccountSID: "AC00000000000000000000000000000001", From: "+15005550006", WebhookURL: hookURL + "sms"}.Extension()},
		model.Channel{Key: "whatsapp", Type: model.TypeTwilioWhatsapp, CredentialRef: "twilio",
			Configuration: twiliowhatsapp.Config{AccountSID: "AC00000000000000000000000000000001", From: "+15005550006", WebhookURL: hookURL + "whatsapp"}.Extension()},
		model.Channel{Key: "meta", Type: model.TypeMetaWhatsapp, CredentialRef: "meta",
			Configuration: metawhatsapp.Config{PhoneNumberID: "100000000000001"}.Extension()},
		model.Channel{Key: "disc", Type: model.TypeDiscordChat, CredentialRef: "discord",
			Configuration: discord.Config{ApplicationID: "900000000000000001", BotUserID: "900000000000000002"}.Extension()},
		model.Channel{Key: "chat", Type: model.TypeSlackChat, CredentialRef: "slack",
			Configuration: slack.Config{TeamID: "T0000000001", BotUserID: "U0000000001"}.Extension()},
		model.Channel{Key: "bot", Type: model.TypeTelegramMessages, CredentialRef: "telegram",
			Configuration: telegram.Config{BotID: 7000000001, BotUsername: "heo_test_bot"}.Extension()},
		model.Channel{Key: "teams", Type: model.TypeMicrosoftTeams, CredentialRef: "teams",
			Configuration: teams.Config{AppID: teamsTestApp, TenantID: teamsTestTenant, ServiceURL: "https://smba.trafficmanager.net/teams/"}.Extension()},
	)
	s := newSandbox(t)
	s.useStore(db)
	hook, mx := freeAddr(t), freeAddr(t)
	d := s.start(nil, nil, []string{"socket", "workers", "receivers", "webhook"},
		"-c", "services.webhook.addr="+hook, "-c", composition.KeySMTPAddr+"="+mx,
		"-c", "services.receivers.start_spread=0s",
		"service", "start", "--enable", "webhook")
	if code, body := httpGet(t, "http://"+hook+"/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d %s", code, body)
	}

	route := func(key string) string { return "http://" + hook + "/webhooks/" + string(ids[key]) }
	for _, tc := range []struct {
		key, method, path, contentType, body string
		want                                 int
	}{
		// No credential stored: the adapters that verify with one answer
		// unavailable, so the provider retries once it is.
		{"widget", http.MethodPost, "/session?site=site-1", "application/json", "{}", http.StatusServiceUnavailable},
		{"widget", http.MethodGet, composition.WidgetEventsRoute, "", "", http.StatusServiceUnavailable},
		{"sms", http.MethodPost, "", "application/x-www-form-urlencoded", "Body=hi", http.StatusServiceUnavailable},
		{"whatsapp", http.MethodPost, "", "application/x-www-form-urlencoded", "Body=hi", http.StatusServiceUnavailable},
		{"meta", http.MethodGet, "?hub.mode=subscribe&hub.verify_token=x&hub.challenge=1", "", "", http.StatusServiceUnavailable},
		{"chat", http.MethodPost, "", "application/json", `{"type":"url_verification","challenge":"abc"}`, http.StatusServiceUnavailable},
		// No signature: refused before any credential is read.
		{"disc", http.MethodPost, "", "application/json", `{"type":1}`, http.StatusUnauthorized},
		// No secret token header: refused before any credential is read.
		{"bot", http.MethodPost, "", "application/json", `{"update_id":1}`, http.StatusUnauthorized},
		// No Bot Connector token: refused before the Bot Framework's keys
		// are read, and with no credential of heo's, which receiving needs
		// none of.
		{"teams", http.MethodPost, "", "application/json", `{"type":"message"}`, http.StatusUnauthorized},
		{"gmail", http.MethodPost, "", "application/json", "{}", http.StatusUnauthorized},
		{"outlook", http.MethodPost, "?validationToken=check-1", "text/plain", "", http.StatusOK},
	} {
		req, err := http.NewRequestWithContext(t.Context(), tc.method, route(tc.key)+tc.path, strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		if tc.contentType != "" {
			req.Header.Set("Content-Type", tc.contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s%s = %d, want %d", tc.method, tc.key, tc.path, resp.StatusCode, tc.want)
		}
	}

	conn, err := net.DialTimeout("tcp", mx, 5*time.Second)
	if err != nil {
		t.Fatalf("smtp.mail listener: %v\n%s", err, d.log())
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	greeting, _ := bufio.NewReader(conn).ReadString('\n')
	_ = conn.Close()
	if !strings.HasPrefix(greeting, "220 ") {
		t.Errorf("smtp.mail greeting = %q", greeting)
	}

	// Receivers needing a credential fail by scope; the daemon stays up.
	for _, key := range []string{"mailbox", "gmail", "outlook", "bot", "disc"} {
		scope := "scope=channel:" + string(ids[key])
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(d.log(), scope) {
			if time.Now().After(deadline) {
				t.Fatalf("no report for %s's receiver:\n%s", key, d.log())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if code := d.term(); code != 0 {
		t.Fatalf("SIGTERM exit %d:\n%s", code, d.log())
	}
}
