package e2e

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

// TestChannelCheckRemoteReportsTheReceiveCall: on the e2e build, whose
// fake adapter can report a link and fail a connection with a stated wait,
// `channel check --remote` reports what the daemon's receive call is doing
// next to the lease: a link that is up reads ready, a failure that states
// a wait reads waiting with the failure's code and when the next
// connection opens, and one that cannot be retried reads failed; the
// check exits 0, 0 (a waiting receiver is a warning) and 2, with the
// flag in either position, as JSON and as a table.
func TestChannelCheckRemoteReportsTheReceiveCall(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		for _, ch := range []struct{ key, cfg string }{
			{"up", `{"account":"acct-up","link":"connected"}`},
			{"limited", `{"account":"acct-limited","fail_receive":"unavailable","retry_after":"1h"}`},
			{"refused", `{"account":"acct-refused","fail_receive":"permanent"}`},
		} {
			c.heo(0, "channel", "create", "--type", "test.heo.fake", "--key", ch.key, "--configuration", ch.cfg)
			c.heo(0, "inbox", "create", "--channel", ch.key, "--key", ch.key+"-in", "--destination", "room-"+ch.key, "--mode", model.ModePerRoom)
		}

		type receiving struct {
			Way, State, Reason, Owner, Phase, Link, LastFailure string
			Failures                                            int
			Until, RetryAt                                      time.Time
		}
		type report struct {
			OK     bool
			Checks []struct {
				Findings []struct{ Severity, Scope, Code string }
				Remote   struct{ Receiving []map[string]any }
			}
		}
		check := func(exit int, args ...string) (receiving, map[string]any, report) {
			t.Helper()
			var rep report
			r := c.heo(exit, append([]string{"channel", "check"}, append(args, "--format", "json")...)...)
			if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil || len(rep.Checks) != 1 {
				t.Fatalf("%v\n%s", err, r.stdout)
			}
			for _, raw := range rep.Checks[0].Remote.Receiving {
				if raw["way"] == "receive" {
					data, _ := json.Marshal(raw)
					var rv receiving
					_ = json.Unmarshal(data, &rv)
					rv.LastFailure, _ = raw["last_failure"].(string)
					rv.RetryAt, _ = time.Parse(time.RFC3339, str(raw["retry_at"]))
					return rv, raw, rep
				}
			}
			t.Fatalf("no receive way in %s", r.stdout)
			return receiving{}, nil, rep
		}

		// The link is up: ready, running, connected, the lease held, no failure.
		var rv receiving
		var raw map[string]any
		eventually(t, "the link to read connected", func() bool {
			rv, raw, _ = check(0, "up", "--remote")
			return rv.State == "ready" && rv.Link == "connected"
		})
		if rv.Phase != "running" || rv.Owner == "" || rv.Until.IsZero() || rv.Failures != 0 || !rv.RetryAt.IsZero() {
			t.Errorf("up = %+v, want running with the lease and no failure", raw)
		}
		for _, k := range []string{"failures", "last_failure", "retry_at"} {
			if _, ok := raw[k]; ok {
				t.Errorf("a healthy receiver reports %s: %+v", k, raw)
			}
		}
		check(0, "--remote", "up") // the flag in the other position

		// A failure that states an hour's wait: waiting, retrying at the hour, a
		// warning and exit 0.
		var rep report
		eventually(t, "the receiver to read retrying", func() bool {
			rv, raw, rep = check(0, "limited", "--remote")
			return rv.Phase == "retrying"
		})
		if rv.State != "waiting" || rv.Failures != 1 || rv.LastFailure != "unavailable" || rv.Owner == "" ||
			time.Until(rv.RetryAt) < 59*time.Minute || time.Until(rv.RetryAt) > time.Hour+time.Minute ||
			!strings.Contains(rv.Reason, "unavailable") || !strings.Contains(rv.Reason, rv.RetryAt.UTC().Format(time.RFC3339)) {
			t.Errorf("limited = %+v, want waiting after one unavailable, retrying in about an hour", raw)
		}
		warned := false
		for _, f := range rep.Checks[0].Findings {
			warned = warned || (f.Scope == "receiving:receive" && f.Code == "receiver_waiting" && f.Severity == "warning")
		}
		if !rep.OK || !warned {
			t.Errorf("limited check = %+v, want ok with a warning for the waiting receiver", rep)
		}
		check(0, "--remote", "limited")
		table := c.heo(0, "channel", "check", "--remote", "limited").stdout
		for _, want := range []string{`receiving\s+receive\s+waiting\s+unavailable: retrying at \S+ after 1 failure lease \S+ until \S+; phase retrying, failures 1 \(last unavailable\), retry at \S+`} {
			if !regexp.MustCompile(want).MatchString(table) {
				t.Errorf("table lacks %q:\n%s", want, table)
			}
		}

		// A failure that cannot be retried: failed, no call left, an error finding,
		// exit 2.
		eventually(t, "the receiver to read failed", func() bool {
			rv, raw, rep = check(2, "refused", "--remote")
			return rv.State == "failed"
		})
		if rv.Phase != "" || rv.Link != "" || rv.Owner != "" || rv.Reason == "" {
			t.Errorf("refused = %+v, want failed with the reason and no call", raw)
		}
		failed := false
		for _, f := range rep.Checks[0].Findings {
			failed = failed || (f.Scope == "receiving:receive" && f.Code == "receiver_failed" && f.Severity == "error")
		}
		if rep.OK || !failed {
			t.Errorf("refused check = %+v, want an error finding", rep)
		}
		check(2, "--remote", "refused")
		table = c.heo(2, "channel", "check", "--remote", "refused").stdout
		if !regexp.MustCompile(`receiving\s+receive\s+failed\s+`).MatchString(table) || strings.Contains(table, "phase") {
			t.Errorf("table = %q, want failed and no call", table)
		}

		// The daemon's own view agrees: waiting is degraded, not unready.
		st := c.runtimeOf()
		for _, s := range st.Services {
			if s.Name == "receivers" && s.State != "degraded" {
				t.Errorf("receivers = %s, want degraded", s.State)
			}
		}
		c.stop(d)
	})
}

func str(v any) string { s, _ := v.(string); return s }
