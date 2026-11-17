package e2e

import (
	"errors"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// refusal is what a refused command said: the error's code line and
// message, which stderr starts with, and what to do next. It is what a
// preview and a submission must agree on.
func refusal(r result) string { return strings.TrimSpace(r.stderr) }

// reconcileArgs spells `delivery reconcile id` with the flags in each
// position: before the verb, and after the operation, in text and JSON.
func reconcileArgs(id model.ID, dry bool, format string, flagsFirst bool) []string {
	var flags []string
	if dry {
		flags = append(flags, "--dry-run")
	}
	if format != "" {
		flags = append(flags, "--format", format)
	}
	verb := []string{"delivery", "reconcile", string(id)}
	if flagsFirst {
		return append(append([]string{}, flags...), verb...)
	}
	return append(verb, flags...)
}

// TestReconcilePreviewAgreesWithSubmission drives the e2e build with the
// fake adapter: three channels each hold an outcome_unknown delivery. A
// channel whose adapter declares no reconcile, one whose configuration
// leaves it off (as a relay with no lookup URL does) and one whose
// provider permission is withdrawn are refused by `delivery reconcile`;
// `--dry-run` is refused the same way, with the same exit code and the
// same message, in text and in JSON, with the flags in either position,
// and asks the provider nothing. A channel that can reconcile is
// previewed (exit 0) and its real run asks the provider.
func TestReconcilePreviewAgreesWithSubmission(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start("workers")
		c.fakeChannel("plain", adapterstest.Config{Account: "acct-rp-plain"})
		c.fakeChannel("off", adapterstest.Config{Account: "acct-rp-off", Disabled: []model.Capability{model.CapabilityReconcile}})
		c.fakeChannel("lost", adapterstest.Config{Account: "acct-rp-lost", Withdrawn: []model.Capability{model.CapabilityReconcile}})
		for _, key := range []string{"plain", "off", "lost"} {
			c.heo(0, "inbox", "create", "--channel", key, "--key", key, "--destination", key+"-1",
				"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		}

		// One ambiguous send through each channel, from a worker of the
		// test's own whose connection drops after the request.
		fake := adapterstest.New()
		fake.EnableReconcile()
		e := c.db.engine([]adapter.Factory{fake}, func(cfg *uma.Config) {
			cfg.Work = &uma.WorkPolicy{MaxClaims: 10, MaxInFlight: 8}
			cfg.Retry = uma.RetryPolicy{MaxAttempts: 5, BaseDelay: 300 * time.Millisecond, MaxDelay: 300 * time.Millisecond}
		})
		fake.FailSends(errors.New("connection reset after write"))
		unknown := map[string]model.ID{}
		for _, key := range []string{"plain", "off", "lost"} {
			var s sendJSON
			c.json(&s, "message", "send", "--inbox", key, "--to", "user:"+key, "--text", "hi "+key, "--idempotency-key", "rp-"+key)
			unknown[key] = s.Operation.ID
		}
		stop := worker(t, e, "rp-a", 2*time.Second)
		for _, id := range unknown {
			c.waitDelivery(id, model.Unknown)
		}
		c.stopWorker(stop)
		fake.FailSends(nil)

		// agree runs the preview and the submission, in every spelling of
		// the preview, and requires one answer: the exit code and what
		// stderr says.
		agree := func(what string, id model.ID, exit int, code string) string {
			t.Helper()
			var text string
			for _, format := range []string{"", "json"} {
				real := c.heo(exit, reconcileArgs(id, false, format, false)...)
				wantStderr(t, real, code)
				want := refusal(real)
				if format == "" {
					text = want
				}
				for _, first := range []bool{false, true} {
					args := reconcileArgs(id, true, format, first)
					got := c.heo(exit, args...)
					if refusal(got) != want {
						t.Errorf("%s: preview %v said\n%s\nthe submission said\n%s", what, args, refusal(got), want)
					}
					if strings.Contains(got.stdout, "operation:") || strings.Contains(got.stdout, `"kind"`) {
						t.Errorf("%s: preview %v planned an effect it was refused:\n%s", what, args, got.stdout)
					}
				}
			}
			if got := c.delivery(id); got.State != model.Unknown {
				t.Errorf("%s: the delivery is %s after the refusals", what, got.State)
			}
			return text
		}

		// The adapter declares no reconcile: the daemon runs without the
		// fake declaring it.
		msg := agree("an adapter that declares none", unknown["plain"], 2, "UNSUPPORTED")
		if !strings.Contains(msg, "nothing will reconcile it") {
			t.Errorf("the refusal does not say nothing will reconcile it:\n%s", msg)
		}

		// The adapter declares reconcile: a channel whose configuration
		// leaves it off, or whose permission is withdrawn, still refuses.
		c.stop(d)
		c.s.env["HEO_E2E_FAKE_RECONCILE"] = "1"
		d = c.start("workers")
		msg = agree("a channel that leaves it off", unknown["off"], 2, "UNSUPPORTED")
		for _, want := range []string{"reconcile unavailable", "leaves it off"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the refusal lacks %q:\n%s", want, msg)
			}
		}
		msg = agree("a channel whose permission is withdrawn", unknown["lost"], 5, "UNAUTHORIZED")
		if !strings.Contains(msg, "withdrawn") {
			t.Errorf("the refusal does not say the permission is withdrawn:\n%s", msg)
		}

		// A channel that can reconcile is previewed in either position and
		// in both formats, and asks nothing; its submission asks the
		// provider, which the fake says cannot be reached.
		for _, format := range []string{"", "json"} {
			for _, first := range []bool{false, true} {
				r := c.heo(0, reconcileArgs(unknown["plain"], true, format, first)...)
				if !strings.Contains(r.stdout, "operation:"+string(unknown["plain"])) || (format == "json" && !strings.Contains(r.stdout, "nothing is sent")) {
					t.Errorf("preview %v of a channel that can reconcile:\n%s", reconcileArgs(unknown["plain"], true, format, first), r.stdout)
				}
			}
		}
		wantStderr(t, c.heo(6, reconcileArgs(unknown["plain"], false, "", false)...), "UNAVAILABLE")
		if got := c.delivery(unknown["plain"]); got.State != model.Unknown {
			t.Errorf("the delivery is %s", got.State)
		}
		c.stop(d)
	})
}
