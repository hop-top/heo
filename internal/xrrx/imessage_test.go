package xrrx

import (
	"io"
	"net/http"
	"strings"
	"testing"

	xrr "hop.top/xrr"
)

// imessageEnv is a valid recording environment of backend.
func imessageEnv(t *testing.T, backend string, mod map[string]string) func(string) string {
	t.Helper()
	env := map[string]string{
		xrr.EnvMode: string(xrr.ModeRecord), EnvLive: "1", EnvIMessageBackend: backend, EnvIMessageTo: "Maintainer@Example.com",
		EnvIMessageListen: "127.0.0.1:8787", EnvIMessageLedger: t.TempDir(),
	}
	switch backend {
	case IMessageBackendBlooio:
		env[EnvBlooioAPIKey], env[EnvBlooioFrom], env[EnvBlooioWebhookSecret] = "api_key_value", "+15550109001", "whsec_value0123456789"
	default:
		env[EnvBlueBubblesURL], env[EnvBlueBubblesPassword] = "http://192.0.2.10:1234", "server-password"
		env[EnvBlueBubblesAccount], env[EnvBlueBubblesWebhookToken] = "mac-owner@example.com", "webhook-token-0123456789"
	}
	for k, v := range mod {
		if v == "" {
			delete(env, k)
			continue
		}
		env[k] = v
	}
	return func(k string) string { return env[k] }
}

// TestIMessageEnvironmentChecked: a live recording needs record mode, the
// live flag, no CI, a backend with all its credentials, one handle that is
// not the line itself, and the webhook address; the recipient's email
// address is kept in lower case.
func TestIMessageEnvironmentChecked(t *testing.T) {
	for _, backend := range []string{IMessageBackendBlooio, IMessageBackendBlueBubbles} {
		if err := CheckLiveIMessage(imessageEnv(t, backend, nil)); err != nil {
			t.Fatalf("%s: a valid environment is refused: %v", backend, err)
		}
		s, err := IMessageSandboxFromEnv(imessageEnv(t, backend, nil))
		if err != nil || s.To != "maintainer@example.com" || s.Max != DefaultIMessageMaxSends || s.Rehearse {
			t.Fatalf("%s: sandbox = %+v, %v", backend, s, err)
		}
		bad := map[string]map[string]string{
			"replay":        {xrr.EnvMode: string(xrr.ModeReplay)},
			"not live":      {EnvLive: ""},
			"CI":            {"CI": "true"},
			"no recipient":  {EnvIMessageTo: ""},
			"a name":        {EnvIMessageTo: "Maintainer"},
			"a short phone": {EnvIMessageTo: "+1555"},
			"no listener":   {EnvIMessageListen: ""},
			"a cap":         {EnvIMessageMaxSends: "many"},
		}
		if backend == IMessageBackendBlooio {
			bad["no key"] = map[string]string{EnvBlooioAPIKey: ""}
			bad["no secret"] = map[string]string{EnvBlooioWebhookSecret: ""}
			bad["a line that is not a number"] = map[string]string{EnvBlooioFrom: "line"}
			bad["the line itself"] = map[string]string{EnvIMessageTo: "+15550109001"}
		} else {
			bad["no password"] = map[string]string{EnvBlueBubblesPassword: ""}
			bad["a url with a path"] = map[string]string{EnvBlueBubblesURL: "http://192.0.2.10:1234/api"}
			bad["a url with credentials"] = map[string]string{EnvBlueBubblesURL: "http://u:p@192.0.2.10:1234"} // #nosec G101 -- a made-up URL
			bad["a short token"] = map[string]string{EnvBlueBubblesWebhookToken: "short"}
			bad["the account itself"] = map[string]string{EnvIMessageTo: "Mac-Owner@example.com"}
		}
		for name, mod := range bad {
			if err := CheckLiveIMessage(imessageEnv(t, backend, mod)); err == nil {
				t.Errorf("%s: %s is accepted", backend, name)
			}
		}
	}
	if err := CheckLiveIMessage(imessageEnv(t, "imessage", nil)); err == nil {
		t.Error("an unknown backend is accepted")
	}
}

type imSendSpy struct{ n int }

func (s *imSendSpy) RoundTrip(*http.Request) (*http.Response, error) {
	s.n++
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"m-1"}`))}, nil
}

func imPost(t *testing.T, rt http.RoundTripper, url, body string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

// TestIMessageTransportAllowsOneSend: the backend's send call to the one
// recipient from the one line, with a short plain text starting "heo
// adapter test", is the only request that leaves, and each takes a ledger
// slot first; a read, another path or host, another recipient, a field the
// send does not carry and a link are refused before anything is sent, and
// the cap holds across transports sharing a ledger.
func TestIMessageTransportAllowsOneSend(t *testing.T) {
	text := IMessageText
	blooio := func(mod string) string {
		return `{"from":"+15550109001","to":"` + map[bool]string{true: "+15550109002", false: "maintainer@example.com"}[mod == "other"] +
			`","text":"` + map[bool]string{true: "heo adapter test http://x.example", false: text}[mod == "link"] + `"` + map[bool]string{true: `,"attachments":["x"]`, false: ""}[mod == "extra"] + `}`
	}
	bb := func(chat, msg string) string {
		return `{"chatGuid":"` + chat + `","tempGuid":"t-1","message":"` + msg + `","method":"apple-script"}`
	}
	env := imessageEnv(t, IMessageBackendBlooio, map[string]string{EnvIMessageMaxSends: "2"})
	s, err := IMessageSandboxFromEnv(env)
	if err != nil {
		t.Fatal(err)
	}
	spy := &imSendSpy{}
	rt := s.Transport(spy)
	for name, c := range map[string]struct{ url, body string }{
		"another recipient": {IMessageBlooioBase + "/messages", blooio("other")},
		"a link":            {IMessageBlooioBase + "/messages", blooio("link")},
		"an extra field":    {IMessageBlooioBase + "/messages", blooio("extra")},
		"another host":      {"https://evil.example.com/v4/messages", blooio("")},
		"another path":      {IMessageBlooioBase + "/me/numbers", blooio("")},
		"http":              {"http://api.blooio.com/v4/messages", blooio("")},
		"not JSON":          {IMessageBlooioBase + "/messages", "from=x"},
		"another text":      {IMessageBlooioBase + "/messages", strings.Replace(blooio(""), text, "hello there", 1)},
	} {
		if err := imPost(t, rt, c.url, c.body); err == nil {
			t.Errorf("%s: sent", name)
		}
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, IMessageBlooioBase+"/messages", nil)
	if _, err := rt.RoundTrip(req); err == nil {
		t.Error("a read was sent")
	}
	if spy.n != 0 || s.Sent() != 0 {
		t.Fatalf("refused requests reached the backend (%d) or took a slot (%d)", spy.n, s.Sent())
	}
	for i := 1; i <= 2; i++ {
		if err := imPost(t, rt, IMessageBlooioBase+"/messages", blooio("")); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	other, _ := IMessageSandboxFromEnv(env)
	for name, rt := range map[string]http.RoundTripper{"the same transport": rt, "another one on the ledger": other.Transport(spy)} {
		if err := imPost(t, rt, IMessageBlooioBase+"/messages", blooio("")); err == nil || !strings.Contains(err.Error(), "cap of 2") {
			t.Errorf("%s: a third send: %v, want the cap", name, err)
		}
	}
	if spy.n != 2 || s.Sent() != 2 {
		t.Fatalf("sent %d, ledger %d, want 2 and 2", spy.n, s.Sent())
	}

	// BlueBubbles: the AppleScript send to the recipient's one-to-one chat.
	bbEnv := imessageEnv(t, IMessageBackendBlueBubbles, nil)
	b, err := IMessageSandboxFromEnv(bbEnv)
	if err != nil {
		t.Fatal(err)
	}
	spy = &imSendSpy{}
	rt = b.Transport(spy)
	send := b.Base + "/api/v1/message/text?password=server-password"
	for name, c := range map[string]struct{ url, body string }{
		"another chat":    {send, bb("iMessage;-;other@example.com", text)},
		"a group chat":    {send, bb("iMessage;+;chat123", text)},
		"another server":  {"http://192.0.2.99:1234/api/v1/message/text", bb("iMessage;-;maintainer@example.com", text)},
		"another path":    {b.Base + "/api/v1/chat/new", bb("iMessage;-;maintainer@example.com", text)},
		"another message": {send, bb("iMessage;-;maintainer@example.com", "hello")},
	} {
		if err := imPost(t, rt, c.url, c.body); err == nil {
			t.Errorf("bluebubbles %s: sent", name)
		}
	}
	if err := imPost(t, rt, send, bb("iMessage;-;Maintainer@example.com", text)); err != nil {
		t.Fatalf("bluebubbles send: %v", err)
	}
	if spy.n != 1 {
		t.Errorf("bluebubbles sent %d, want 1", spy.n)
	}
}

// TestIMessageRehearsalReachesNothing: a rehearsal answers from canned
// shapes, takes no slot and sends nothing, and still refuses what a
// recording would.
func TestIMessageRehearsalReachesNothing(t *testing.T) {
	s, err := IMessageSandboxFromEnv(imessageEnv(t, IMessageBackendBlooio, map[string]string{EnvIMessageRehearse: "1", EnvIMessageMaxSends: "0"}))
	if err != nil || !s.Rehearse {
		t.Fatalf("sandbox = %+v, %v", s, err)
	}
	spy := &imSendSpy{}
	rt := s.Transport(spy)
	good := `{"from":"+15550109001","to":"maintainer@example.com","text":"` + IMessageText + `"}`
	if err := imPost(t, rt, IMessageBlooioBase+"/messages", good); err != nil {
		t.Fatalf("rehearsed send: %v", err)
	}
	if err := imPost(t, rt, IMessageBlooioBase+"/messages", strings.Replace(good, "maintainer", "someone", 1)); err == nil {
		t.Error("a rehearsal sent to another recipient")
	}
	if spy.n != 0 || s.Sent() != 0 {
		t.Errorf("a rehearsal reached the backend (%d) or took a slot (%d)", spy.n, s.Sent())
	}
}

// TestIMessageDenyNamesWhatMustNotBeKept: the audit's literals hold the
// recipient, the line and every credential of the backend.
func TestIMessageDenyNamesWhatMustNotBeKept(t *testing.T) {
	s, _ := IMessageSandboxFromEnv(imessageEnv(t, IMessageBackendBlooio, nil))
	deny := strings.Join(s.Deny(), " ")
	for _, want := range []string{"maintainer@example.com", "15550109001", "api_key_value", "whsec_value0123456789"} {
		if !strings.Contains(deny, want) {
			t.Errorf("Deny() lacks %q: %v", want, s.Deny())
		}
	}
	b, _ := IMessageSandboxFromEnv(imessageEnv(t, IMessageBackendBlueBubbles, nil))
	deny = strings.Join(b.Deny(), " ")
	for _, want := range []string{"server-password", "mac-owner@example.com", "webhook-token-0123456789", "192.0.2.10:1234"} {
		if !strings.Contains(deny, want) {
			t.Errorf("Deny() lacks %q: %v", want, b.Deny())
		}
	}
}
