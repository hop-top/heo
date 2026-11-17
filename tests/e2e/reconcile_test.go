package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/composition"
	rt "hop.top/heo/internal/daemon"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

// eventually polls cond until it holds, failing after 15s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: never held", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// runtimeOf asks the profile's daemon (pre selects it) for its runtime
// state.
func (c *cliSite) runtimeOf(pre ...string) rt.Status {
	c.t.Helper()
	var p struct{ Runtime rt.Status }
	c.json(&p, append(pre, "daemon", "ping")...)
	return p.Runtime
}

// scopeOf returns svc's scope for channel id in the daemon pre selects.
func (c *cliSite) scopeOf(svc string, id model.ID, pre ...string) (rt.ScopeStatus, bool) {
	c.t.Helper()
	for _, s := range c.runtimeOf(pre...).Services {
		if s.Name != svc {
			continue
		}
		for _, sc := range s.Scopes {
			if sc.Scope == "channel:"+string(id) {
				return sc, true
			}
		}
	}
	return rt.ScopeStatus{}, false
}

// fakeChannel creates a fake channel on account through heo and returns
// it as stored.
func (c *cliSite) fakeChannel(key string, cfg adapterstest.Config) model.Channel {
	c.t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		c.t.Fatal(err)
	}
	var out channelOutJSON
	c.json(&out, "channel", "create", "--type", string(adapterstest.Type), "--key", key, "--configuration", string(b))
	return out.Channel
}

// updateChannel replaces ch's configuration through heo and returns it
// as stored.
func (c *cliSite) updateChannel(ch model.Channel, configuration string) model.Channel {
	c.t.Helper()
	var out channelOutJSON
	c.json(&out, "channel", "update", ch.Key, "--revision", string(ch.Revision), "--configuration", configuration)
	return out.Channel
}

// hookPost posts to a channel's route and returns the status and the
// revision the fake adapter answered at.
func hookPost(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get(adapterstest.RevisionHeader)
}

// heldOpen opens the fake adapter's held-open route on url and returns
// a channel closed when the response ends.
func heldOpen(t *testing.T, url string) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+adapterstest.StreamRoute, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); resp.StatusCode != http.StatusOK || err != nil || line != "open\n" {
		t.Fatalf("held-open route = %d %q %v", resp.StatusCode, line, err)
	}
	ended := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, br)
		close(ended)
	}()
	return ended
}

// ended waits for a widget event stream to end, failing after 15s.
func ended(t *testing.T, events <-chan sseEvent) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the event stream on the replaced route never ended")
		}
	}
}

// TestRuntimeReconcilesChannelsLive: the running daemon applies channel
// and inbox changes as heo commits them, without a restart. A widget
// channel and its inbox created while it runs receive a visitor; a fake
// channel added is routed and received; updated, its receiver and route
// are replaced and the request open on the old route ends; withdrawn
// from the webhook, its route is gone; a channel on an account another
// profile's daemon receives fails on its own scope. Through all of it
// the widget visitor's event stream stays open and still delivers. An
// inbox disabled refuses inbound at once, and the widget channel
// updated to a second site serves it, while the stream on its old route
// ends and resumes on the new one.
func TestRuntimeReconcilesChannelsLive(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		other := newDB(t).migrate()
		other.channel("other", "held", "acct-shared")
		c.s.profileStore("other", other)
		held := c.s.start(nil, nil, []string{"receivers"}, "--profile", "other", "service", "start", "receivers")
		t.Cleanup(func() { held.term() })
		d := c.start()
		hooks := "http://" + c.hook + "/webhooks/"

		// A widget channel and its inbox, created live, receive.
		var web channelOutJSON
		c.json(&web, "channel", "create", "--type", string(widget.Type), "--key", "web",
			"--configuration", `{"sites":["site-1"]}`, "--credential-ref", "widget-key")
		var in inboxOutJSON
		c.json(&in, "inbox", "create", "--channel", "web", "--key", "webin", "--destination", "site-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		site := &widgetSite{t: t, db: c.db, s: c.s, hook: c.hook, route: hooks + string(web.Channel.Ref.ID)}
		token, conv := site.visit()
		events := widgetEvents(t, site.route, token, "")
		op := c.db.operator("default")
		reply(t, op, conv, "before")
		first := next(t, events)
		if !strings.Contains(first.data, `"before"`) {
			t.Fatalf("event = %+v", first)
		}

		// A fake channel, created live: routed and received.
		alpha := c.fakeChannel("alpha", adapterstest.Config{Account: "acct-a"})
		route := hooks + string(alpha.Ref.ID)
		if code, rev := hookPost(t, route); code != http.StatusNoContent || rev != string(alpha.Revision) {
			t.Fatalf("new channel's route = %d at %q, want 204 at %s", code, rev, alpha.Revision)
		}
		eventually(t, "new channel receiving", func() bool {
			sc, ok := c.scopeOf(rt.NameReceivers, alpha.Ref.ID)
			return ok && sc.State == rt.ScopeReady
		})
		old := heldOpen(t, route)

		// Updated: replaced, the old route's request ends.
		alpha = c.updateChannel(alpha, `{"account":"acct-a2"}`)
		if code, rev := hookPost(t, route); code != http.StatusNoContent || rev != string(alpha.Revision) {
			t.Errorf("updated channel's route = %d at %q, want 204 at %s", code, rev, alpha.Revision)
		}
		select {
		case <-old:
		case <-time.After(10 * time.Second):
			t.Error("the request open on the replaced route never ended")
		}

		// Withdrawn from the webhook: its route is gone, its receiver runs.
		alpha = c.updateChannel(alpha, `{"account":"acct-a2","no_webhook":true}`)
		if code, _ := hookPost(t, route); code != http.StatusNotFound {
			t.Errorf("withdrawn route = %d, want 404", code)
		}
		if sc, ok := c.scopeOf(rt.NameWebhook, alpha.Ref.ID); ok {
			t.Errorf("webhook scope left for an unmounted channel: %+v", sc)
		}

		// An account another profile's daemon receives: this channel
		// fails on its scope, the others run on.
		beta := c.fakeChannel("beta", adapterstest.Config{Account: "acct-shared"})
		sc, ok := c.scopeOf(rt.NameReceivers, beta.Ref.ID)
		if !ok || sc.State != rt.ScopeFailed || !strings.Contains(sc.Reason, "already received by heo/other (pid ") {
			t.Errorf("claimed account's scope = %+v, %v", sc, ok)
		}
		if code, _ := hookPost(t, hooks+string(beta.Ref.ID)); code != http.StatusServiceUnavailable {
			t.Errorf("claimed account's route = %d, want 503", code)
		}
		st := c.runtimeOf()
		if !st.Ready {
			t.Errorf("runtime not ready with one failed channel: %+v", st)
		}
		if sc, ok := c.scopeOf(rt.NameReceivers, alpha.Ref.ID); !ok || sc.State != rt.ScopeReady {
			t.Errorf("unaffected channel = %+v, %v", sc, ok)
		}

		// The widget stream was never interrupted.
		reply(t, op, conv, "after")
		if ev := next(t, events); !strings.Contains(ev.data, `"after"`) {
			t.Fatalf("event after the changes = %+v", ev)
		}

		// Inbox disabled: inbound refused at once; enabled: accepted.
		var off inboxOutJSON
		c.json(&off, "inbox", "disable", "webin", "--revision", string(in.Inbox.Revision))
		if code, body := postJSON(t, site.route+"/messages", `{"id":"m-2","text":"off"}`, "Authorization", "Bearer "+token); code != http.StatusUnprocessableEntity {
			t.Errorf("message to a disabled inbox = %d %s, want 422", code, body)
		}
		var on inboxOutJSON
		c.json(&on, "inbox", "enable", "webin", "--revision", string(off.Inbox.Revision))
		if code, body := postJSON(t, site.route+"/messages", `{"id":"m-3","text":"on"}`, "Authorization", "Bearer "+token); code != http.StatusAccepted {
			t.Errorf("message once enabled = %d %s, want 202", code, body)
		}

		// The widget channel updated to a second site, and its inbox to
		// receive it: the new site serves, the old stream ends and
		// resumes on the new route.
		session := site.route + "/session?" + composition.WidgetSiteParam + "=site-2"
		if code, _ := postJSON(t, session, "{}"); code == http.StatusOK {
			t.Fatal("a site the channel does not serve opened a session")
		}
		var webNow model.Channel
		c.json(&webNow, "channel", "get", "web")
		var upd channelOutJSON
		c.json(&upd, "channel", "update", "web", "--revision", string(webNow.Revision),
			"--configuration", `{"sites":["site-1","site-2"]}`)
		c.json(&in, "inbox", "update", "webin", "--revision", string(on.Inbox.Revision), "--destination", "site-1,site-2")
		if code, body := postJSON(t, session, "{}"); code != http.StatusOK {
			t.Errorf("second site's session after the update = %d %s", code, body)
		}
		ended(t, events)
		reply(t, op, conv, "resumed")
		if ev := next(t, widgetEvents(t, site.route, token, first.id)); ev.name != widget.EventMessage {
			t.Errorf("resumed stream = %+v", ev)
		}
		c.stop(d)
	})
}
