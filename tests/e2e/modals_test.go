package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/model"
)

type modalJSON struct {
	Effect       string
	Interaction  model.ID
	Conversation model.ID
	Inbox        model.ID
	ViewID       string `json:"view_id"`
	ExternalID   string `json:"external_id"`
	Hash         string
	TriggerUntil *time.Time `json:"trigger_until"`
	Operation    opJSON
	OperationKey string `json:"operation_key"`
	Notes        []string
}

// viewsSite is a daemon with a fake channel that reports interactions the
// way Slack does and ships Slack's three modal effects, recording each in
// a log.
type viewsSite struct {
	*cliSite
	d       *daemon
	effects string
	vw      model.Channel
}

const (
	modalFile = `{
  "type": "modal",
  "external_id": "deploy-42",
  "title": {"type": "plain_text", "text": "Approve"},
  "submit": {"type": "plain_text", "text": "Go"},
  "blocks": [{"type": "input", "block_id": "why", "label": {"type": "plain_text", "text": "Why"}, "element": {"type": "plain_text_input", "action_id": "why"}}]
}`
	// modalWindow is how long the fake provider gives a press: the
	// real 3 seconds are the adapter's (internal/daemon), and a test
	// that drives a built binary cannot be held to them.
	modalWindow = time.Minute
)

func newViewsSite(t *testing.T, profile string) *viewsSite {
	t.Helper()
	c := newCLISite(t, noBridge)
	s := &viewsSite{cliSite: c, effects: c.s.path("effects.log")}
	c.s.env["HEO_E2E_FAKE_EFFECT_LOG"] = s.effects
	if profile != "" {
		c.s.write("cfg/heo/profiles/default.yaml", profile)
	}
	s.d = c.start()
	s.vw = c.fakeOf(adapterstest.ViewsType, "vw", adapterstest.Config{Account: "acct-vw"})
	c.heo(0, "inbox", "create", "--channel", "vw", "--key", "vwin", "--destination", "vw-1",
		"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
	return s
}

// press has the provider report a button press that carried trigger_id
// trigger (none when empty) at time at, answerable for window, and returns
// the interaction heo holds.
func (s *viewsSite) press(trigger string, at time.Time, window time.Duration) model.Interaction {
	s.t.Helper()
	event := "action:" + trigger
	if trigger == "" {
		event = "action:sha256-0123456789abcdef"
	}
	s.inject(s.vw, adapterstest.Inject{
		Event: event, From: "ada", Destination: "vw-1", OccurredAt: at,
		Interaction: &adapterstest.InjectInteraction{Action: "approve", AcknowledgeFor: window},
	})
	for _, in := range interactionsOf(s.cliSite) {
		if slices.Contains(in.External, model.ProviderInteractionRef(s.vw.Ref, event)) {
			return in
		}
	}
	s.t.Fatalf("press %s not listed", event)
	return model.Interaction{}
}

func (s *viewsSite) applied() []string {
	b, _ := os.ReadFile(s.effects)
	t := strings.TrimSpace(string(b))
	if t == "" {
		return nil
	}
	return strings.Split(t, "\n")
}

func (s *viewsSite) ledger() int {
	s.t.Helper()
	c := s.db.operator("default")
	req := model.ListRequest{Call: model.Call{Correlation: "e2e"}, Page: model.PageRequest{Limit: 100}}
	n := 0
	for {
		p, err := c.Operations().List(s.t.Context(), req)
		if err != nil {
			s.t.Fatal(err)
		}
		n += len(p.Items)
		if p.Complete || p.Next == "" {
			return n
		}
		req.Page.After = p.Next
	}
}

func (s *viewsSite) viewFile(body string) string {
	s.t.Helper()
	p := filepath.Join(s.t.TempDir(), "view.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		s.t.Fatal(err)
	}
	return p
}

// viewOfSize is a modal view of exactly n bytes once compact that
// the adapter's offline rules accept: a hundred sections, the most it
// allows, the last one padded.
func viewOfSize(n int) string {
	const blocks = 100
	head := `{"type":"modal","title":{"type":"plain_text","text":"T"},"blocks":[`
	section := func(text string) string {
		return `{"type":"section","text":{"type":"mrkdwn","text":"` + text + `"}}`
	}
	size := len(head) + len(`]}`) + (blocks - 1) // the commas
	fixed := len(section(""))
	filler := (n - size - blocks*fixed) / blocks
	var b strings.Builder
	b.WriteString(head)
	used := size + blocks*fixed
	for i := 0; i < blocks; i++ {
		l := filler
		if i == blocks-1 {
			l = n - used - (blocks-1)*filler
		}
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(section(strings.Repeat("s", l)))
	}
	b.WriteString("]}")
	return b.String()
}

// viewOf is the ID of the view a settled operation reports.
func (s *viewsSite) viewOf(op model.ID) string {
	s.t.Helper()
	var d struct {
		ProviderReferences []model.ExternalRef `json:"provider_references"`
	}
	s.json(&d, "delivery", "get", string(op))
	for _, r := range d.ProviderReferences {
		if r.Namespace == slack.Namespace && r.Kind == slack.RefKindView {
			return r.ID
		}
	}
	s.t.Fatalf("delivery %s reports no view: %+v", op, d.ProviderReferences)
	return ""
}

// TestModalLifecycle drives the e2e build on Postgres with a fake provider
// that reports Slack-shaped interactions and ships Slack's modal effects.
// A preview names the external change, the window the interaction's own
// trigger_id gives it and what was checked of the adapter, shows no
// trigger_id, and writes nothing; the open is queued durably under its
// key and replays, reaches the provider with the interaction's trigger_id
// and the view, and the view it opened comes back as the operation's
// provider reference, which an update by that ID names; an update by
// external_id, with the view on standard input and the flags in either
// position, and a push do the same. The view is read where the caller is
// (a file, stdin, the JSON itself), a large one goes through the socket,
// and the exit codes of every refusal are the grammar's: an interaction
// past its window 1, a redacted one 4, an unknown one 3, a bad view or
// flag 2, a viewer 5, with nothing written.
func TestModalLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		s := newViewsSite(t, noRateLimit)
		file := s.viewFile(modalFile)
		in := s.press("398738663015.47445629121.open", time.Now(), modalWindow)
		id := string(in.Ref.ID)

		// The preview.
		open := []string{"interaction", "open-modal", id, "--view", file}
		var plan planJSON
		s.json(&plan, append(open, "--dry-run")...)
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "send" || plan.Effects[0].Target != "slack:modal interaction:"+id {
			t.Fatalf("preview effects = %+v", plan.Effects)
		}
		for _, want := range []string{"external", "modal", "slack.chat.views.open", "queued", "woken", "stops working"} {
			if !strings.Contains(plan.Effects[0].Detail, want) {
				t.Errorf("effect detail lacks %q: %s", want, plan.Effects[0].Detail)
			}
		}
		for k, want := range map[string]string{"visibility": `"internal"`, "adapter": `"` + string(adapterstest.ViewsType) + `"`, "interaction": `"` + id + `"`} {
			if string(plan.Args[k]) != want {
				t.Errorf("preview arg %s = %s, want %s", k, plan.Args[k], want)
			}
		}
		var line string
		_ = json.Unmarshal(plan.Args["capability"], &line)
		for _, want := range []string{"extension_execute declared by " + string(adapterstest.ViewsType), "ships slack.chat.views.open", "live availability not checked"} {
			if !strings.Contains(line, want) {
				t.Errorf("capability line %q lacks %q", line, want)
			}
		}
		txt := s.heo(0, append(open, "--dry-run")...)
		for _, want := range []string{"capability", "interaction", "queued, not shown"} {
			if !strings.Contains(txt.stdout, want) {
				t.Errorf("text preview lacks %q:\n%s", want, txt.stdout)
			}
		}
		raw := s.heo(0, append(open, "--dry-run", "--format", "json")...)
		if strings.Contains(raw.stdout+raw.stderr, "398738663015") {
			t.Errorf("the preview shows the trigger_id:\n%s%s", raw.stdout, raw.stderr)
		}
		start := s.ledger()
		if n := s.ledger(); n != start || len(s.applied()) != 0 {
			t.Fatalf("--dry-run left %d operations, was %d; provider saw %v", n, start, s.applied())
		}

		// The open: queued durably, never shown by the command.
		var out, again modalJSON
		began := time.Now()
		s.json(&out, append(open, "--idempotency-key", "open-1")...)
		t.Logf("open-modal, command start to exit: %s", time.Since(began))
		if out.Effect != slack.ExtensionViewsOpen || out.Interaction != in.Ref.ID || out.Conversation != in.Ref.Conversation.ID ||
			out.Operation.State != model.Queued || !out.Operation.Durable || out.Operation.Replayed || out.OperationKey != "open-1" ||
			out.ExternalID != "deploy-42" || out.TriggerUntil == nil || !out.TriggerUntil.Equal(in.Deadline) {
			t.Fatalf("open = %+v", out)
		}
		notes := strings.Join(out.Notes, "\n")
		for _, want := range []string{"heo operation get " + string(out.Operation.ID), "provider reference", "--view-id <id>", "--external-id deploy-42", "never sent again"} {
			if !strings.Contains(notes, want) {
				t.Errorf("the open's notes lack %q:\n%s", want, notes)
			}
		}
		s.json(&again, append(open, "--idempotency-key", "open-1")...)
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replayed open = %+v, first %+v", again, out)
		}
		d := s.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered)
		if d.State == model.Failed {
			t.Fatalf("open failed: %+v", d.Failure)
		}
		t.Logf("open-modal, command start to settled operation: %s", time.Since(began))
		eventually(t, "the provider was asked to open the view", func() bool { return len(s.applied()) == 1 })
		sent := s.applied()[0]
		if !strings.HasPrefix(sent, "effect slack.chat.views.open ") || !strings.Contains(sent, `"trigger_id":"398738663015.47445629121.open"`) ||
			!strings.Contains(sent, `"expires_at":"`+in.Deadline.UTC().Format(time.RFC3339Nano)+`"`) || !strings.Contains(sent, `"external_id":"deploy-42"`) ||
			strings.Contains(sent, "\n") {
			t.Fatalf("provider was asked %q", sent)
		}

		// The view's ID is on the settled operation.
		view := s.viewOf(out.Operation.ID)
		if !strings.HasPrefix(view, "V") {
			t.Fatalf("view = %q", view)
		}
		r := s.heo(0, "operation", "get", string(out.Operation.ID))
		if !strings.Contains(strings.Join(strings.Fields(r.stdout), " "), "provider refs view "+view) {
			t.Errorf("operation get does not show the view:\n%s", r.stdout)
		}

		// Update by that ID, with the view on standard input and the flags
		// before the command's own; then by external_id.
		next := strings.Replace(modalFile, "Approve", "Approved", 1)
		var up modalJSON
		r = s.s.runIn(next, nil, "--format", "json", "interaction", "update-modal", "--view", "-", "--view-id", view, "--hash", "156772938.1827394",
			"--inbox", "vwin", "--idempotency-key", "up-1")
		s.s.want(r, 0, "interaction", "update-modal")
		if err := json.Unmarshal([]byte(r.stdout), &up); err != nil || up.Effect != slack.ExtensionViewsUpdate || up.ViewID != view || up.Hash != "156772938.1827394" ||
			up.Operation.State != model.Queued || up.TriggerUntil != nil || up.Interaction != "" {
			t.Fatalf("update = %s, %v", r.stdout, err)
		}
		s.waitDelivery(up.Operation.ID, model.ProviderAccepted, model.Delivered)
		var up2 modalJSON
		s.json(&up2, "interaction", "update-modal", "--inbox", "vwin", "--external-id", "deploy-42", "--view", s.viewFile(next), "--idempotency-key", "up-2")
		s.waitDelivery(up2.Operation.ID, model.ProviderAccepted, model.Delivered)
		eventually(t, "the updates reached the provider", func() bool { return len(s.applied()) == 3 })
		lines := s.applied()
		if !strings.HasPrefix(lines[1], "effect slack.chat.views.update ") || !strings.Contains(lines[1], `"view_id":"`+view+`"`) ||
			!strings.Contains(lines[1], `"hash":"156772938.1827394"`) || strings.Contains(lines[1], "trigger_id") ||
			!strings.Contains(lines[2], `"external_id":"deploy-42"`) || strings.Contains(lines[2], "view_id") {
			t.Errorf("provider was asked %q", lines[1:])
		}

		// Push, on another press, and the text form of the open.
		inner := s.press("398738663015.47445629121.push", time.Now(), modalWindow)
		var pushed modalJSON
		s.json(&pushed, "interaction", "push-modal", string(inner.Ref.ID), "--view", file, "--idempotency-key", "push-1")
		if pushed.Effect != slack.ExtensionViewsPush || pushed.Operation.State != model.Queued {
			t.Fatalf("push = %+v", pushed)
		}
		s.waitDelivery(pushed.Operation.ID, model.ProviderAccepted, model.Delivered)
		if !strings.HasPrefix(s.viewOf(pushed.Operation.ID), "V") {
			t.Error("a push reports no view")
		}
		third := s.press("398738663015.47445629121.text", time.Now(), modalWindow)
		r = s.heo(0, "interaction", "open-modal", string(third.Ref.ID), "--view", modalFile, "--idempotency-key", "open-text")
		flat := strings.Join(strings.Fields(r.stdout), " ")
		for _, want := range []string{"effect slack.chat.views.open", "external id deploy-42", "trigger valid until", "operation key open-text", "(queued)"} {
			if !strings.Contains(flat, want) {
				t.Errorf("text open lacks %q:\n%s", want, r.stdout)
			}
		}
		if !strings.Contains(r.stderr, "queued: accepted durably") || !strings.Contains(r.stderr, "provider reference") || !strings.Contains(r.stderr, "update-modal") {
			t.Errorf("text open notes:\n%s", r.stderr)
		}
		eventually(t, "the provider saw the pushes", func() bool { return len(s.applied()) == 5 })

		// A view the size uma bounds passes through the socket.
		big := viewOfSize(slack.MaxViewBytes)
		fresh := s.press("398738663015.47445629121.big0", time.Now(), modalWindow)
		s.json(&plan, "interaction", "open-modal", string(fresh.Ref.ID), "--view", s.viewFile(big), "--dry-run")
		// And one byte over it, as the adapter's bound, is refused offline
		// as a usage error: no daemon, no exit 70.
		over := viewOfSize(slack.MaxViewBytes + 1)
		wantStderr(t, s.heo(2, "interaction", "open-modal", string(fresh.Ref.ID), "--view", s.viewFile(over)), "250 KiB")
		wantStderr(t, s.heo(2, "interaction", "open-modal", string(fresh.Ref.ID), "--view", s.viewFile(over), "--dry-run"), "250 KiB")

		// Usage and validation: before anything is written.
		beforeUsage := s.ledger()
		for name, tc := range map[string]struct {
			args  []string
			stdin string
			want  string
		}{
			"no such file": {args: []string{"--view", filepath.Join(t.TempDir(), "nope.json")}, want: "no such file"},
			"not JSON":     {args: []string{"--view", "-"}, stdin: "type: modal", want: "not valid JSON"},
			"an array":     {args: []string{"--view", "-"}, stdin: "[1]", want: "JSON object"},
			"not a modal":  {args: []string{"--view", s.viewFile(`{"type":"home","title":{"type":"plain_text","text":"T"},"blocks":[]}`)}, want: "VALIDATION"},
		} {
			args := append([]string{"interaction", "open-modal", string(fresh.Ref.ID)}, tc.args...)
			r := s.s.runIn(tc.stdin, nil, args...)
			if r.exit != 2 || !strings.Contains(r.stderr, tc.want) {
				t.Errorf("%s: exit %d, want 2 with %q:\n%s", name, r.exit, tc.want, r.stderr)
			}
		}
		wantStderr(t, s.heo(2, "interaction", "open-modal", string(fresh.Ref.ID), "--view", s.viewFile(`{"type":"home","title":{"type":"plain_text","text":"T"},"blocks":[]}`), "--idempotency-key", "bad-type"), "VALIDATION")
		wantStderr(t, s.heo(2, "interaction", "update-modal", "--inbox", "vwin", "--view-id", "V1", "--external-id", "x", "--view", file), "none of the others", "view-id")
		wantStderr(t, s.heo(2, "interaction", "update-modal", "--inbox", "vwin", "--view", file), "view-id", "external-id")
		wantStderr(t, s.heo(2, "interaction", "update-modal", "--view-id", "V1", "--view", file), "inbox")
		wantStderr(t, s.heo(2, "interaction", "update-modal", "--inbox", "vwin", "--view-id", "not-a-view", "--view", file, "--dry-run"), "VALIDATION", "view_id")
		wantStderr(t, s.heo(3, "interaction", "update-modal", "--inbox", "nope", "--view-id", "V1", "--view", file), "NOT_FOUND")

		if n := s.ledger(); n != beforeUsage || len(s.applied()) != 5 {
			t.Errorf("a usage or validation error wrote: operations %d, was %d; provider %v", n, beforeUsage, s.applied())
		}

		// The interaction: past its window (1), redacted (4), unknown (3),
		// carrying no trigger_id (2); at the preview and on submission.
		stale := s.press("398738663015.47445629121.stal", time.Now().Add(-2*time.Minute), 0)
		for _, extra := range [][]string{{"--dry-run"}, {"--idempotency-key", "stale-1"}} {
			r := s.heo(1, append([]string{"interaction", "open-modal", string(stale.Ref.ID), "--view", file}, extra...)...)
			wantStderr(t, r, "DEADLINE_EXPIRED", "stopped working at", "trigger_id")
		}
		wantStderr(t, s.heo(1, "interaction", "push-modal", string(stale.Ref.ID), "--view", file, "--idempotency-key", "stale-2"), "DEADLINE_EXPIRED")
		red := s.press("398738663015.47445629121.redd", time.Now(), modalWindow)
		op := s.db.operator("default")
		if _, err := op.Privacy().Redact(t.Context(), model.Redact{
			Command: model.Command{Call: model.Call{Correlation: "e2e"}, Operation: "redact-ix", ExpectedRevision: red.Revision},
			Target:  red.Ref.Ref(), Interaction: &red.Ref, Reason: "e2e",
		}); err != nil {
			t.Fatalf("redact: %v", err)
		}
		before := s.ledger() // the redaction is an operation of its own
		for _, extra := range [][]string{{"--dry-run"}, {"--idempotency-key", "red-1"}} {
			wantStderr(t, s.heo(4, append([]string{"interaction", "open-modal", string(red.Ref.ID), "--view", file}, extra...)...), "CONFLICT", "redacted")
		}
		wantStderr(t, s.heo(3, "interaction", "open-modal", "ix_404", "--view", file), "NOT_FOUND")
		wantStderr(t, s.heo(3, "interaction", "open-modal", string(fresh.Ref.ID), "--conversation", "conv_404", "--view", file), "NOT_FOUND")
		bare := s.press("", time.Now(), modalWindow)
		wantStderr(t, s.heo(2, "interaction", "open-modal", string(bare.Ref.ID), "--view", file), "VALIDATION", "carries no trigger_id")
		if n := s.ledger(); n != before || len(s.applied()) != 5 {
			t.Errorf("a refused attempt wrote: operations %d, was %d; provider %v", n, before, s.applied())
		}

		// A viewer may not open, push or update, preview included.
		s.stop(s.d)
		s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
		s.d = s.start()
		for _, args := range [][]string{
			{"interaction", "open-modal", string(fresh.Ref.ID), "--view", file},
			{"interaction", "push-modal", string(fresh.Ref.ID), "--view", file},
			{"interaction", "update-modal", "--inbox", "vwin", "--view-id", view, "--view", file},
		} {
			for _, extra := range [][]string{nil, {"--dry-run"}} {
				wantDenied(t, s.s.run(nil, append(slices.Clone(args), extra...)...), "heo.authz.not_granted")
			}
		}
		if len(s.applied()) != 5 {
			t.Errorf("provider saw %v", s.applied())
		}
		s.stop(s.d)
	})
}

// TestModalRefusals: an adapter whose channel does not declare
// extension_execute, one that declares it for another adapter's effect, and
// one that ships the modal effects are told apart by the verb table, and
// the verbs are refused UNSUPPORTED (exit 2) at the preview and on
// submission, before anything is written, on every channel that lacks the
// very effect each runs.
func TestModalRefusals(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, noBridge)
		d := c.start()
		file := filepath.Join(t.TempDir(), "view.json")
		if err := os.WriteFile(file, []byte(modalFile), 0o600); err != nil {
			t.Fatal(err)
		}
		ix := c.fakeOf(adapterstest.InteractionType, "ix", adapterstest.Config{Account: "acct-ix"})
		c.fakeOf(adapterstest.Type, "plain", adapterstest.Config{Account: "acct-plain"})
		c.fakeOf(adapterstest.UpdatesType, "up", adapterstest.Config{Account: "acct-up"})
		c.fakeOf(adapterstest.ViewsType, "vw", adapterstest.Config{Account: "acct-vw"})
		for _, in := range []struct{ ch, key, dest string }{{"ix", "ixin", "ix-1"}, {"plain", "plin", "pl-1"}, {"up", "upin", "up-1"}} {
			c.heo(0, "inbox", "create", "--channel", in.ch, "--key", in.key, "--destination", in.dest,
				"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		}
		c.inject(ix, adapterstest.Inject{
			Event: "action:398738663015.47445629121.refu", From: "ada", Destination: "ix-1", OccurredAt: time.Now(),
			Interaction: &adapterstest.InjectInteraction{Action: "approve", AcknowledgeFor: modalWindow},
		})
		in := interactionsOf(c)[0]
		id := string(in.Ref.ID)

		// The verb table: declared where the effect is shipped, and the
		// reason where it is not.
		modal := map[string]string{
			"interaction open-modal": "slack.chat.views.open", "interaction push-modal": "slack.chat.views.push", "interaction update-modal": "slack.chat.views.update",
		}
		for key, wantState := range map[string]string{"vw": "declared", "ix": "unsupported", "plain": "unsupported", "up": "unsupported"} {
			var v channelViewJSON
			c.json(&v, "channel", "get", key)
			for verb, effect := range modal {
				got, ok := verbOf(v.Verbs, verb)
				if !ok || got.Capability != "extension_execute" || got.State != wantState {
					t.Errorf("channel %s verb %s = %+v (listed %v), want extension_execute %s", key, verb, got, ok, wantState)
				}
				if key == "up" && !strings.Contains(got.Detail, effect) {
					t.Errorf("channel up verb %s detail %q does not name %s", verb, got.Detail, effect)
				}
			}
		}
		before := len(interactionsOf(c))

		// The refusals.
		for _, extra := range [][]string{{"--dry-run"}, {"--idempotency-key", "refused-1"}} {
			wantStderr(t, c.heo(2, append([]string{"interaction", "open-modal", id, "--view", file}, extra...)...),
				"UNSUPPORTED", "does not declare extension_execute", "interaction open-modal", "heo channel get "+string(ix.Ref.ID))
			wantStderr(t, c.heo(2, append([]string{"interaction", "push-modal", id, "--view", file}, extra...)...),
				"UNSUPPORTED", "interaction push-modal")
		}
		for _, inbox := range []string{"ixin", "plin", "upin"} {
			for _, extra := range [][]string{{"--dry-run"}, {"--idempotency-key", "refused-" + inbox}} {
				args := append([]string{"interaction", "update-modal", "--inbox", inbox, "--view-id", "V0123456789", "--view", file}, extra...)
				want := []string{"UNSUPPORTED", "interaction update-modal"}
				if inbox == "upin" {
					want = append(want, "ships no effect slack.chat.views.update")
				} else {
					want = append(want, "does not declare extension_execute")
				}
				wantStderr(t, c.heo(2, args...), want[0], want[1:]...)
			}
		}
		var ops struct{ Items []json.RawMessage }
		c.json(&ops, "operation", "list", "--kind", model.ActionExtensionExecute)
		if len(ops.Items) != 0 || before != len(interactionsOf(c)) {
			t.Errorf("a refused modal wrote: %d operations", len(ops.Items))
		}
		c.stop(d)
	})
}
