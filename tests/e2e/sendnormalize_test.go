package e2e

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/model"
)

// sendPreviews runs `message send ... --dry-run` with the flag in each
// position and returns the text results; both must agree.
func (c *cliSite) sendPreviews(exit int, send ...string) []result {
	c.t.Helper()
	after := c.heo(exit, append(append([]string{}, send...), "--dry-run")...)
	before := c.heo(exit, append([]string{"--dry-run"}, send...)...)
	if undated(before.stdout) != undated(after.stdout) || before.stderr != after.stderr {
		c.t.Errorf("--dry-run before and after the command disagree:\nbefore: %q %q\nafter:  %q %q",
			before.stdout, before.stderr, after.stdout, after.stderr)
	}
	return []result{after, before}
}

// generated is the line of a text preview that holds the time it was made.
var generated = regexp.MustCompile(`(?m)^GENERATED .*\n`)

func undated(s string) string { return generated.ReplaceAllString(s, "") }

// routeID reads the ID of the route plan.Args[key] holds, and whether
// the plan holds the key at all.
func routeID(t *testing.T, plan planJSON, key string) (string, bool) {
	t.Helper()
	raw, ok := plan.Args[key]
	if !ok {
		return "", false
	}
	var ref model.ExternalRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("plan arg %s = %s: %v", key, raw, err)
	}
	return ref.ID, true
}

var asTypedRow = regexp.MustCompile(`(?m)^as typed +(\S+) (\S+)$`)

// TestMessageSendPreviewNormalizesRecipient drives the e2e build through
// an adapter whose factory implements adapter.RecipientNormalizer (as
// relay.messages does) and one that does not. The preview names the
// recipient under the spelling the real send uses, in text and JSON and
// with the flag in either position, and shows the typed spelling ("as
// typed") only when the two differ: an address in mixed case is
// previewed lower-cased, as typed beside it, and the send lands on that
// route; a recipient the adapter leaves alone, and every recipient of an
// adapter that does not normalize, is previewed as typed. A recipient
// the adapter refuses is refused at the preview as at the send, in the
// same exit code and words, and nothing is written.
func TestMessageSendPreviewNormalizesRecipient(t *testing.T) {
	withE2EBinary(t, func() {
		// The write rate limit is off: the test makes many refused writes.
		c := newCLISite(t, relayRateLimitOff)
		d := c.start()
		norm := c.fakeOf(adapterstest.NormalizerType, "norm", adapterstest.Config{Account: "acct-norm"})
		c.fakeChannel("plain", adapterstest.Config{Account: "acct-plain"})
		for _, in := range []struct{ channel, key string }{{"norm", "normed"}, {"plain", "plained"}} {
			c.heo(0, "inbox", "create", "--channel", in.channel, "--key", in.key, "--destination", "out-"+in.key,
				"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		}
		route := func(id string) model.ExternalRef {
			return model.ExternalRef{Channel: norm.Ref, Namespace: adapterstest.Namespace, Kind: "user", ID: id}
		}
		send := func(inbox, to string) []string {
			return []string{"message", "send", "--inbox", inbox, "--to", to, "--text", "welcome aboard"}
		}
		conversations := func() int {
			var convs pageJSON[model.Conversation]
			c.json(&convs, "conversation", "list")
			return len(convs.Items)
		}

		// A mixed-case address: previewed lower-cased, the typed spelling
		// beside it, in both flag positions and in JSON.
		const typed, lowered = "Bob@Example.com", "bob@example.com"
		for _, r := range c.sendPreviews(0, send("normed", "user:"+typed)...) {
			if !strings.Contains(r.stdout, "destination   user "+lowered) {
				t.Errorf("text preview lacks the normalized destination:\n%s", r.stdout)
			}
			if m := asTypedRow.FindStringSubmatch(r.stdout); m == nil || m[1] != "user" || m[2] != typed {
				t.Errorf("text preview lacks the typed spelling as a row:\n%s", r.stdout)
			}
			// The effect line names the route the send uses, not the typed one.
			if strings.Count(r.stdout, typed) != 1 {
				t.Errorf("the typed spelling shows more than once in the text preview:\n%s", r.stdout)
			}
		}
		var plan planJSON
		for _, args := range [][]string{append(send("normed", "user:"+typed), "--dry-run"), append([]string{"--dry-run"}, send("normed", "user:"+typed)...)} {
			c.json(&plan, args...)
			dest, _ := routeID(t, plan, "destination")
			asTyped, has := routeID(t, plan, "as_typed")
			if dest != lowered || !has || asTyped != typed || len(plan.Effects) != 1 || strings.Contains(plan.Effects[0].Detail, typed) ||
				!strings.Contains(plan.Effects[0].Detail, lowered) {
				t.Errorf("JSON preview %v = destination %q, as typed %q (%v), effects %+v", args, dest, asTyped, has, plan.Effects)
			}
		}
		if n := conversations(); n != 0 {
			t.Fatalf("a preview wrote %d conversations", n)
		}

		// The send goes where the preview said, and the conversation is
		// routed there.
		var sent sendJSON
		c.json(&sent, append(send("normed", "user:"+typed), "--idempotency-key", "norm-1")...)
		if sent.Destination != route(lowered) {
			t.Fatalf("send destination = %+v, want the previewed %+v", sent.Destination, route(lowered))
		}
		var conv model.Conversation
		c.json(&conv, "conversation", "get", string(sent.Conversation))
		if conv.ReplyRoute != route(lowered) {
			t.Fatalf("started conversation routed to %+v, want %+v", conv.ReplyRoute, route(lowered))
		}
		// The same start in the other spelling is the same operation.
		var again sendJSON
		c.json(&again, append(send("normed", "user:"+lowered), "--idempotency-key", "norm-1")...)
		if !again.Operation.Replayed || again.Operation.ID != sent.Operation.ID || again.Conversation != sent.Conversation {
			t.Fatalf("start in the other spelling = %+v, want a replay of %+v", again, sent)
		}

		// A recipient the adapter leaves alone shows no typed row, in text
		// or JSON.
		for _, id := range []string{lowered, "cy", "+15550100003"} {
			for _, r := range c.sendPreviews(0, send("normed", "user:"+id)...) {
				if !strings.Contains(r.stdout, "destination   user "+id) || strings.Contains(r.stdout, "as typed") {
					t.Errorf("preview of %s:\n%s", id, r.stdout)
				}
			}
			c.json(&plan, append(send("normed", "user:"+id), "--dry-run")...)
			if dest, _ := routeID(t, plan, "destination"); dest != id {
				t.Errorf("JSON preview of %s names destination %q", id, dest)
			}
			if _, has := plan.Args["as_typed"]; has {
				t.Errorf("JSON preview of %s carries as_typed: %s", id, plan.Args["as_typed"])
			}
		}

		// An adapter without the interface: the recipient is previewed, and
		// sent, as typed.
		for _, r := range c.sendPreviews(0, send("plained", "user:"+typed)...) {
			if !strings.Contains(r.stdout, "destination   user "+typed) || strings.Contains(r.stdout, "as typed") {
				t.Errorf("preview on an adapter without a normalizer:\n%s", r.stdout)
			}
		}
		c.json(&plan, append(send("plained", "user:"+typed), "--dry-run")...)
		if dest, _ := routeID(t, plan, "destination"); dest != typed {
			t.Errorf("JSON preview on an adapter without a normalizer names destination %q", dest)
		}
		if _, has := plan.Args["as_typed"]; has {
			t.Errorf("JSON preview on an adapter without a normalizer carries as_typed: %s", plan.Args["as_typed"])
		}
		var plainSent sendJSON
		c.json(&plainSent, send("plained", "user:"+typed)...)
		if plainSent.Destination.ID != typed {
			t.Errorf("send on an adapter without a normalizer went to %q, want %q", plainSent.Destination.ID, typed)
		}

		// A refusal: the same exit code and words at the preview, in both
		// flag positions, as at the send; nothing is written.
		before := conversations()
		for _, tc := range []struct {
			id   string
			exit int
			want []string
		}{
			{adapterstest.RefusePrefix + "x", 2, []string{"VALIDATION", "recipient: " + adapterstest.RefuseMessage}},
			{adapterstest.RefuseUnsafePrefix + "x", 2, []string{"VALIDATION", "recipient: refused by the adapter"}},
			{adapterstest.FaultKindPrefix + "x", 1, []string{"PROVIDER_PERMANENT", "adapter returned an invalid normalized recipient"}},
			{adapterstest.FaultEmptyPrefix + "x", 1, []string{"PROVIDER_PERMANENT", "adapter returned an invalid normalized recipient"}},
		} {
			sub := c.s.run(nil, send("normed", "user:"+tc.id)...)
			if sub.exit != tc.exit {
				t.Fatalf("send to %s exit %d, want %d:\n%s", tc.id, sub.exit, tc.exit, sub.stderr)
			}
			wantStderr(t, sub, tc.want[0], tc.want[1:]...)
			if strings.Contains(sub.stderr, "10.0.0.7") {
				t.Errorf("send to %s leaks the adapter's own error:\n%s", tc.id, sub.stderr)
			}
			for _, r := range c.sendPreviews(sub.exit, send("normed", "user:"+tc.id)...) {
				wantStderr(t, r, tc.want[0], tc.want[1:]...)
				if r.stderr != sub.stderr {
					t.Errorf("preview of %s refused differently than the send:\npreview: %s\nsend:    %s", tc.id, r.stderr, sub.stderr)
				}
			}
		}
		if after := conversations(); after != before {
			t.Fatalf("refused starts wrote conversations: %d before, %d after", before, after)
		}
		c.stop(d)
	})
}
