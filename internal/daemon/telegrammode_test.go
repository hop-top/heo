package daemon_test

import (
	"net/http"
	"testing"
	"time"

	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/uma/adapter/chat/telegram"
	"hop.top/uma/model"
)

// receiveMode sets the channel's way of receiving.
func receiveMode(m telegram.ReceiveMode) func(*telegram.Config) {
	return func(c *telegram.Config) { c.Receive = m }
}

// webhookScope is the webhook service's scope for the channel, and
// whether it has one.
func (s *telegramSite) webhookScope() (daemon.ScopeStatus, bool) {
	s.t.Helper()
	return s.r.scope(daemon.NameWebhook, s.ch)
}

// healthy fails unless both inbound services read ready, not degraded.
func (s *telegramSite) healthy() {
	s.t.Helper()
	st := s.r.host.Status()
	for _, name := range []string{daemon.NameReceivers, daemon.NameWebhook} {
		if got := service(st, name).State; got != daemon.StateReady {
			s.t.Errorf("%s = %s, want ready", name, got)
		}
	}
}

// TestTelegramReceiveWebhookOnly: a channel whose configuration receives
// by webhook gets its route and no receiver. heo never reads getUpdates
// for it, the receivers service holds no scope for it and is not
// degraded: receive is off by configuration, not failing.
func TestTelegramReceiveWebhookOnly(t *testing.T) {
	s := newTelegramSite(t, fullTelegramCreds(t), telegramConfig(receiveMode(telegram.ReceiveWebhook)),
		daemon.NameReceivers, daemon.NameWebhook)
	if code, body := s.update(tgMessage(30, telegramRoom, 300, telegramPerson, "by webhook")); code != http.StatusOK {
		t.Fatalf("update = %d %q", code, body)
	}
	if got := s.messages(s.roomConversation(telegramRoom).Ref); len(got) != 1 || desk.Text(got[0].Content) != "by webhook" {
		t.Fatalf("messages = %+v, want the delivered one", got)
	}
	if sc, ok := s.webhookScope(); !ok || sc.State != daemon.ScopeReady {
		t.Errorf("webhook scope = %+v, %v, want ready", sc, ok)
	}
	time.Sleep(300 * time.Millisecond)
	if sc, ok := s.r.scope(daemon.NameReceivers, s.ch); ok {
		t.Errorf("receivers scope for a webhook channel: %+v", sc)
	}
	if n := len(s.api.called("getUpdates")); n != 0 {
		t.Errorf("heo read getUpdates %d times for a webhook channel", n)
	}
	s.healthy()
}

// TestTelegramReceivePollOnly: a channel whose configuration polls gets
// its receiver and no route: a request to where the route would be is
// 404, the webhook service holds no scope for it, and neither service is
// degraded.
func TestTelegramReceivePollOnly(t *testing.T) {
	s := newTelegramSite(t, fullTelegramCreds(t), telegramConfig(receiveMode(telegram.ReceivePoll)),
		daemon.NameReceivers, daemon.NameWebhook)
	s.api.enqueue(tgMessage(40, telegramRoom, 400, telegramPerson, "by polling"))
	waitFor(t, func() bool { return len(s.conversations()) == 1 })
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeReady })
	if code, _ := s.update(tgMessage(41, telegramRoom, 401, telegramPerson, "by webhook")); code != http.StatusNotFound {
		t.Errorf("route of a polling channel = %d, want 404: nothing is mounted", code)
	}
	if sc, ok := s.webhookScope(); ok {
		t.Errorf("webhook scope for a polling channel: %+v", sc)
	}
	s.healthy()
}

// TestTelegramReceiveEitherWay: with no receive mode set the channel
// offers both ways and heo runs both, as before receive modes: a
// receiver and a route.
func TestTelegramReceiveEitherWay(t *testing.T) {
	s := newTelegramSite(t, fullTelegramCreds(t), telegramConfig(), daemon.NameReceivers, daemon.NameWebhook)
	waitFor(t, func() bool { return len(s.api.called("getUpdates")) > 0 })
	if sc := s.receiverScope(); sc.State != daemon.ScopeReady {
		t.Errorf("receivers scope = %+v, want ready", sc)
	}
	if sc, ok := s.webhookScope(); !ok || sc.State != daemon.ScopeReady {
		t.Errorf("webhook scope = %+v, %v, want ready", sc, ok)
	}
	if code, body := s.update(tgMessage(50, telegramRoom, 500, telegramPerson, "by webhook")); code != http.StatusOK {
		t.Errorf("update = %d %q", code, body)
	}
}

// TestTelegramReceiveModeReconciled: a channel moved from polling to its
// webhook while the daemon runs stops polling and gets its route, and
// moved back, loses the route and polls again; neither service is ever
// left degraded by the way the channel no longer uses.
func TestTelegramReceiveModeReconciled(t *testing.T) {
	s := newTelegramSite(t, fullTelegramCreds(t), telegramConfig(receiveMode(telegram.ReceivePoll)),
		daemon.NameReceivers, daemon.NameWebhook)
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeReady })

	s.reconfigure(telegramConfig(receiveMode(telegram.ReceiveWebhook)))
	waitFor(t, func() bool { _, ok := s.r.scope(daemon.NameReceivers, s.ch); return !ok })
	reads := len(s.api.called("getUpdates"))
	if code, body := s.update(tgMessage(60, telegramRoom, 600, telegramPerson, "by webhook")); code != http.StatusOK {
		t.Fatalf("update after the move to webhook = %d %q", code, body)
	}
	if sc, ok := s.webhookScope(); !ok || sc.State != daemon.ScopeReady {
		t.Errorf("webhook scope after the move = %+v, %v", sc, ok)
	}
	time.Sleep(time.Second + 300*time.Millisecond) // past one long poll
	if n := len(s.api.called("getUpdates")); n > reads+1 {
		t.Errorf("heo kept polling a webhook channel: %d reads, then %d", reads, n)
	}
	s.healthy()

	s.reconfigure(telegramConfig(receiveMode(telegram.ReceivePoll)))
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeReady })
	if code, _ := s.update(tgMessage(61, telegramRoom, 601, telegramPerson, "by webhook")); code != http.StatusNotFound {
		t.Errorf("route after the move back to polling = %d, want 404", code)
	}
	if sc, ok := s.webhookScope(); ok {
		t.Errorf("webhook scope after the move back: %+v", sc)
	}
	s.healthy()
}

// reconfigure stores cfg as the channel's configuration and reconciles
// the running daemon, as `channel update` does.
func (s *telegramSite) reconfigure(cfg telegram.Config) {
	s.t.Helper()
	ch := s.r.stored(s.ch)
	ch.Configuration = cfg.Extension()
	cmd := s.r.cmd("reconfigure-" + string(ch.Revision))
	cmd.ExpectedRevision = ch.Revision
	if _, err := s.r.client().Configuration().ConfigureChannel(s.t.Context(), model.ConfigureChannel{Command: cmd, Channel: ch}); err != nil {
		s.t.Fatalf("update channel: %v", err)
	}
	s.r.reconcile()
}
