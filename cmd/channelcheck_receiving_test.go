package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/channels"
	"hop.top/uma/model"
)

func receivingCheck(states ...channels.ReceivingState) channelChecks {
	return channelChecks{Remote: true, OK: true, Checks: []channels.ChannelCheck{{
		ID: "ch-1", Key: "bot", Type: model.TypeDiscordChat, OK: true,
		Remote: &channels.Remote{Receiving: states},
	}}}
}

// TestChannelCheckTextShowsTheReceiveCall: the table puts what uma
// reports of the receive call next to the lease: phase and link when
// connected, the failures and the next connection when retrying, and the
// owner and expiry alone for a lease another daemon holds.
func TestChannelCheckTextShowsTheReceiveCall(t *testing.T) {
	until := time.Date(2026, 10, 15, 12, 1, 0, 0, time.UTC)
	retry := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		rv   channels.ReceivingState
		want string
	}{
		{"connected", channels.ReceivingState{Way: "receive", State: "ready", Owner: "heo/acme/receiver", Until: until, Phase: "running", Link: "connected"},
			"receiving  receive  ready  lease heo/acme/receiver until 2026-10-15T12:01:00Z; phase running, link connected"},
		{"no reporter", channels.ReceivingState{Way: "receive", State: "ready", Owner: "o", Until: until, Phase: "running"},
			"lease o until 2026-10-15T12:01:00Z; phase running"},
		{"retrying", channels.ReceivingState{Way: "receive", State: "waiting", Reason: "rate_limited: retrying at 2026-10-15T13:00:00Z after 1 failure",
			Owner: "o", Until: until, Phase: "retrying", Failures: 1, LastFailure: "rate_limited", RetryAt: retry},
			"lease o until 2026-10-15T12:01:00Z; phase retrying, failures 1 (last rate_limited), retry at 2026-10-15T13:00:00Z"},
		{"another daemon's lease", channels.ReceivingState{Way: "receive", State: "waiting", Reason: "receiver lease held by other until 2026-10-15T12:01:00Z", Owner: "other", Until: until},
			"lease other until 2026-10-15T12:01:00Z\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeChecks(&out, receivingCheck(tc.rv)); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("table:\n%s\nwant it to contain %q", out.String(), tc.want)
			}
		})
	}
	var out bytes.Buffer
	_ = writeChecks(&out, receivingCheck(channels.ReceivingState{Way: "receive", State: "waiting", Owner: "other", Until: until}))
	if strings.Contains(out.String(), "phase") || strings.Contains(out.String(), "link") || strings.Contains(out.String(), "failures") {
		t.Errorf("a lease another daemon holds shows a call:\n%s", out.String())
	}
}

// TestChannelCheckJSONKeysOfTheReceiveCall: remote.receiving carries
// phase, link, failures, last_failure and retry_at beside owner and
// until, and leaves them out when there is no call.
func TestChannelCheckJSONKeysOfTheReceiveCall(t *testing.T) {
	retry := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
	data, err := json.Marshal(receivingCheck(
		channels.ReceivingState{Way: "receive", State: "waiting", Owner: "o", Until: retry, Phase: "retrying", Link: "connecting", Failures: 2, LastFailure: "unavailable", RetryAt: retry},
		channels.ReceivingState{Way: "receive_http", State: "ready", Path: "/webhooks/x"},
	))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Checks []struct {
			Remote struct{ Receiving []map[string]any }
		}
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	r := got.Checks[0].Remote.Receiving
	for k, want := range map[string]any{"phase": "retrying", "link": "connecting", "failures": 2.0, "last_failure": "unavailable",
		"retry_at": "2026-10-15T13:00:00Z", "owner": "o", "until": "2026-10-15T13:00:00Z"} {
		if r[0][k] != want {
			t.Errorf("receive.%s = %v, want %v", k, r[0][k], want)
		}
	}
	for _, k := range []string{"phase", "link", "failures", "last_failure", "retry_at"} {
		if _, ok := r[1][k]; ok {
			t.Errorf("a route reports %s", k)
		}
	}
}
