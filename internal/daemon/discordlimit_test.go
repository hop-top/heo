package daemon_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/daemon"
	"hop.top/uma"
	"hop.top/uma/model"
)

// TestDiscordGatewayIdentifyLimit: with no Identify left in Discord's daily
// budget the adapter never identifies (past it Discord resets the bot
// token) and fails with the wait until the reset. uma's receiver waits
// it out from the first failure instead of asking again every few
// seconds: the channel reads waiting at once, with the failure's code and
// the time of the next connection, and Get Gateway Bot is not called again
// meanwhile.
func TestDiscordGatewayIdentifyLimit(t *testing.T) {
	api := newDiscordAPI(t, discordToken, discordBot, discordApp)
	api.setStarts(0, time.Hour)
	r := newRig(t, "acme", withComposition(), withCredentials(fullDiscordCreds(t)), withHTTP(api.client()))
	ch := r.channelOf(model.Channel{Key: "bot", Type: model.TypeDiscordChat, CredentialRef: discordRef, Configuration: discordConfig(api).Extension()})
	run := r.run(5*time.Second, daemon.NameReceivers)
	run.ready(daemon.NameReceivers)
	waitFor(t, func() bool { sc, _ := r.scope(daemon.NameReceivers, ch); return sc.State == daemon.ScopeWaiting })
	sc, _ := r.scope(daemon.NameReceivers, ch)
	got := r.host.Receiving(t.Context(), ch, model.CapabilityReceive)
	if !strings.Contains(sc.Reason, string(model.RateLimited)) || !strings.Contains(sc.Reason, got.RetryAt.UTC().Format(time.RFC3339)) {
		t.Errorf("reason = %q, want the rate limit and the time of the next connection", sc.Reason)
	}
	if got.Phase != string(uma.ReceiverRetrying) || got.Failures != 1 || got.LastFailure != string(model.RateLimited) ||
		time.Until(got.RetryAt) < 59*time.Minute || time.Until(got.RetryAt) > time.Hour {
		t.Errorf("receiving = %+v, want retrying after the limit, in about an hour", got)
	}
	reads := len(api.called(http.MethodGet, "gateway/bot"))
	time.Sleep(3500 * time.Millisecond) // past three of the minimum waits
	if n := len(api.called(http.MethodGet, "gateway/bot")); n != reads {
		t.Errorf("Get Gateway Bot was called %d times, then %d, while Discord said to wait an hour", reads, n)
	}
	if ids, _, _ := api.counts(); ids != 0 {
		t.Errorf("identified %d times with no Identify left", ids)
	}
}
