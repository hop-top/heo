package daemon_test

import (
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/serve"
	"hop.top/uma/adapter/chat/telegram"
	"hop.top/uma/model"
)

// TestTelegramPolling: with no webhook set, heo's receiver reads the
// bot's updates by long polling, with the bot token in the request path
// and only the update types the adapter reads: a message opens its
// chat's conversation, a button press is an interaction, and a read
// confirms what the one before it handled, so a restarted daemon resumes
// after the last update recorded instead of handling the queue again.
func TestTelegramPolling(t *testing.T) {
	creds := fullTelegramCreds(t)
	s := newTelegramSite(t, creds, telegramConfig(), daemon.NameReceivers)
	s.api.enqueue(tgMessage(10, telegramRoom, 100, telegramPerson, "polled hello"))
	s.api.enqueue(tgPress(11, telegramRoom, 100, telegramPerson, "cb-9", "approve:9"))
	waitFor(t, func() bool { return len(s.conversations()) == 1 && len(s.interactions()) == 1 })
	conv := s.roomConversation(telegramRoom)
	if got := s.messages(conv.Ref); len(got) != 1 || desk.Text(got[0].Content) != "polled hello" {
		t.Fatalf("messages = %+v, want the polled message", got)
	}
	if in := s.interactions()[0]; in.Action != "approve:9" {
		t.Errorf("interaction = %+v", in)
	}
	for _, c := range s.api.called("getUpdates") {
		if c.Host != "api.telegram.org" || c.Token != telegramToken {
			t.Errorf("getUpdates went to %s with token %q", c.Host, c.Token)
		}
		if allowed, _ := c.Body["allowed_updates"].([]any); len(allowed) != len(telegram.AllowedUpdates) {
			t.Errorf("getUpdates asked for %v, want %v", allowed, telegram.AllowedUpdates)
		}
	}
	if !creds.asked(telegramRef, telegram.CredentialPurposeAPI) {
		t.Error("the bot token was never asked of heo's backend")
	}
	// The next read confirms update 11, then Telegram forgets both.
	waitFor(t, func() bool { return s.api.pending() == 0 })
	if res := s.s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("stop: %s %v", res.Outcome, res.Err)
	}

	// An update that arrives while the daemon is down is read after the
	// ones handled; a restart does not hand the handled ones over again.
	s.api.enqueue(tgMessage(12, telegramRoom, 101, telegramPerson, "polled while down"))
	before := len(s.api.called("getUpdates"))
	again := s.r.run(5*time.Second, daemon.NameReceivers)
	again.ready(daemon.NameReceivers)
	waitFor(t, func() bool { return len(s.messages(conv.Ref)) == 2 })
	if got := s.api.called("getUpdates")[before]; got.Body["offset"] != float64(12) {
		t.Errorf("first read after the restart used offset %v, want 12 (after the last update recorded)", got.Body["offset"])
	}
	if n := len(s.interactions()); n != 1 {
		t.Errorf("a restart recorded the press again: %d interactions", n)
	}
}

// receiverScope is the receivers service's scope for the channel.
func (s *telegramSite) receiverScope() daemon.ScopeStatus {
	s.t.Helper()
	for _, sc := range service(s.r.host.Status(), daemon.NameReceivers).Scopes {
		if sc.Scope == "channel:"+string(s.ch.ID) {
			return sc
		}
	}
	return daemon.ScopeStatus{}
}

// TestTelegramPollingConflict: while a webhook is set at Telegram it
// answers every getUpdates 409. heo does not take that for a receiver
// lease held elsewhere and does not delete the webhook: the channel reads
// waiting with Telegram's reason, the service degraded, and each read is
// further from the last than the one before it, instead of one every
// minimum interval. The webhook is set before the daemon starts: a
// receiver reads ready once its first read is answered, so a webhook set
// after it would be refused only at the second.
func TestTelegramPollingConflict(t *testing.T) {
	s := newTelegramSiteBefore(t, fullTelegramCreds(t), telegramConfig(), model.ModePerRoom,
		func(api *telegramAPI) { api.setWebhook(true) }, daemon.NameReceivers)
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeWaiting })
	sc := s.receiverScope()
	if !strings.Contains(sc.Reason, "webhook") || strings.Contains(sc.Reason, "lease") {
		t.Errorf("reason = %q, want Telegram's webhook conflict, not a lease", sc.Reason)
	}
	if st := service(s.r.host.Status(), daemon.NameReceivers); st.State != daemon.StateDegraded {
		t.Errorf("receivers = %s, want degraded", st.State)
	}
	var at []time.Time
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		at = at[:0]
		for _, c := range s.api.called("getUpdates") {
			at = append(at, c.At)
		}
		if len(at) >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(at) < 3 {
		t.Fatalf("%d reads, want 3 to compare the waits", len(at))
	}
	first, second := at[1].Sub(at[0]), at[2].Sub(at[1])
	if second < first*3/2 {
		t.Errorf("waits between reads = %v then %v, want each longer than the last", first, second)
	}
	if n := len(s.api.called("deleteWebhook")); n != 0 {
		t.Errorf("heo called deleteWebhook %d times: the webhook is the operator's", n)
	}
}

// TestTelegramPollingResumesAfterWebhook: once the webhook is deleted at
// Telegram the receiver's next read succeeds, and the channel reads ready
// and receives.
func TestTelegramPollingResumesAfterWebhook(t *testing.T) {
	s := newTelegramSite(t, fullTelegramCreds(t), telegramConfig(), daemon.NameReceivers)
	s.api.setWebhook(true)
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeWaiting })
	s.api.setWebhook(false)
	s.api.enqueue(tgMessage(20, telegramRoom, 200, telegramPerson, "after the webhook"))
	waitFor(t, func() bool { return len(s.conversations()) == 1 })
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeReady })
	if st := service(s.r.host.Status(), daemon.NameReceivers); st.State != daemon.StateReady {
		t.Errorf("receivers = %s, want ready", st.State)
	}
}

// TestTelegramPollingBadToken: Telegram refuses a revoked token or a token
// naming no bot; the receiver is not retried, as the credential will not
// mend itself, and the channel reads failed with the reason, the rest of
// the daemon unaffected.
func TestTelegramPollingBadToken(t *testing.T) {
	creds := slackCreds(t, "acme", telegramRef, map[string]string{
		telegram.CredentialPurposeAPI: "7000000001:AAHrevoked-token-for-tests", telegram.CredentialPurposeWebhook: telegramSecret, // #nosec G101 -- test value
	})
	s := newTelegramSite(t, creds, telegramConfig(), daemon.NameReceivers)
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeFailed })
	if sc := s.receiverScope(); !strings.Contains(sc.Reason, "bot token") {
		t.Errorf("reason = %q, want the refused bot token", sc.Reason)
	}
	reads := len(s.api.called("getUpdates"))
	time.Sleep(1500 * time.Millisecond)
	if n := len(s.api.called("getUpdates")); n != reads {
		t.Errorf("a refused token was read with again: %d reads, then %d", reads, n)
	}
	sc := s.receiverScope()
	for _, c := range s.api.called("getUpdates") {
		if strings.Contains(sc.Reason+sc.Path+sc.URL, c.Token) {
			t.Error("the bot token reached the scope status")
		}
	}
}
