package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/desk"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

type respondJSON struct {
	Interaction  model.ID
	Conversation model.ID
	Inbox        model.ID
	Mode         string
	Visibility   model.Visibility
	FollowUp     bool      `json:"follow_up"`
	ExpiresAt    time.Time `json:"expires_at"`
	Message      model.ID
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

// interactionsOf lists the interactions of args.
func interactionsOf(c *cliSite, args ...string) []model.Interaction {
	c.t.Helper()
	var p pageJSON[model.Interaction]
	c.json(&p, append([]string{"interaction", "list"}, args...)...)
	return p.Items
}

// TestInteractionRespondLifecycle drives the e2e build on Postgres with a
// fake provider that reports interactions and declares no respond, the
// receive-only account of a chat provider. heo lists, shows and filters
// what it reported, and the event is in the log; answering is refused
// exit 2 (UNSUPPORTED) at the preview and on submission, with nothing
// written; and the usage errors, a missing record and a viewer's refusal
// exit as documented.
func TestInteractionRespondLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, noBridge)
		d := c.start()
		ix := c.fakeOf(adapterstest.InteractionType, "ix", adapterstest.Config{Account: "acct-ix"})
		c.heo(0, "inbox", "create", "--channel", "ix", "--key", "ixin", "--destination", "ix-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		c.inject(ix, adapterstest.Inject{
			Event: "ix-ev-1", From: "ada", Destination: "ix-1",
			Interaction: &adapterstest.InjectInteraction{Action: "approve", Message: "pm-ctl"},
		})
		c.inject(ix, adapterstest.Inject{
			Event: "ix-ev-2", From: "cy", Destination: "ix-1",
			Interaction: &adapterstest.InjectInteraction{Kind: model.InteractionCommand, Command: "help", Arguments: "billing"},
		})

		// Listing and showing.
		all := interactionsOf(c)
		if len(all) != 2 {
			t.Fatalf("interactions = %+v, want two", all)
		}
		approve, help := all[0], all[1]
		if approve.Kind != model.InteractionAction || approve.Action != "approve" || help.Command != "help" || help.Arguments != "billing" {
			t.Fatalf("interactions = %+v / %+v", approve, help)
		}
		if got := interactionsOf(c, "--conversation", string(help.Ref.Conversation.ID)); len(got) != 1 || got[0].Ref != help.Ref {
			t.Errorf("list --conversation = %+v", got)
		}
		if got := interactionsOf(c, "--inbox", "ixin"); len(got) != 2 {
			t.Errorf("list --inbox = %+v", got)
		}
		if got := interactionsOf(c, "--actor", string(approve.Actor.Identity.ID)); len(got) != 1 || got[0].Ref != approve.Ref {
			t.Errorf("list --actor = %+v", got)
		}
		txt := c.heo(0, "interaction", "list")
		for _, want := range []string{"approve", "help", "action", "command", "no"} {
			if !strings.Contains(txt.stdout, want) {
				t.Errorf("text list lacks %q:\n%s", want, txt.stdout)
			}
		}
		var got model.Interaction
		c.json(&got, "interaction", "get", string(approve.Ref.ID))
		if got.Ref != approve.Ref || got.Action != "approve" || got.MessageExternal == nil || got.MessageExternal.ID != "pm-ctl" {
			t.Fatalf("interaction get = %+v", got)
		}
		detail := c.heo(0, "interaction", "get", string(approve.Ref.ID), "--conversation", string(approve.Ref.Conversation.ID))
		for _, want := range []string{"action", "approve", "pm-ctl", "respond until", "the provider states none", "answered", "no"} {
			if !strings.Contains(detail.stdout, want) {
				t.Errorf("text get lacks %q:\n%s", want, detail.stdout)
			}
		}
		if strings.Contains(detail.stdout, "fake-response-token") {
			t.Errorf("get shows the response token:\n%s", detail.stdout)
		}
		wantStderr(t, c.heo(3, "interaction", "get", "ix_404"), "NOT_FOUND")
		wantStderr(t, c.heo(3, "interaction", "get", string(approve.Ref.ID), "--conversation", "conv_404"), "NOT_FOUND")

		// The event the provider's report committed is in the log, next to
		// the message events.
		var evs pageJSON[model.Event]
		c.json(&evs, "event", "list", "--kind", model.EventInteractionReceived)
		if len(evs.Items) != 2 || evs.Items[0].Resource.Kind != model.KindInteraction || evs.Items[0].Resource.ID != approve.Ref.ID {
			t.Fatalf("interaction.received events = %+v", evs.Items)
		}

		// The channel's verb table: respond is not declared.
		var ch struct {
			Verbs []struct{ Verb, State, Reason, Capability string }
		}
		c.json(&ch, "channel", "get", "ix")
		var seen bool
		for _, v := range ch.Verbs {
			if v.Verb == "interaction respond" {
				seen = true
				if v.State != "unsupported" || v.Reason != "not_declared" || v.Capability != "respond" {
					t.Errorf("channel verb = %+v", v)
				}
			}
		}
		if !seen {
			t.Errorf("channel get lists no interaction respond verb: %+v", ch.Verbs)
		}

		// Answering is refused, with nothing written.
		before := (&effectsSite{cliSite: c}).ledger()
		id := string(approve.Ref.ID)
		respond := []string{"interaction", "respond", id, "--mode", "message", "--text", "Approved."}
		wantStderr(t, c.heo(2, append(respond, "--dry-run")...), "UNSUPPORTED", "does not declare respond", "interaction respond", "heo channel get "+string(ix.Ref.ID))
		wantStderr(t, c.heo(2, append(respond, "--idempotency-key", "ans-1")...), "UNSUPPORTED")
		wantStderr(t, c.heo(2, append(respond, "--follow-up", "--dry-run")...), "UNSUPPORTED", "respond")
		if n := (&effectsSite{cliSite: c}).ledger(); n != before {
			t.Errorf("a refused response wrote: %d operations, was %d", n, before)
		}
		var after model.Interaction
		c.json(&after, "interaction", "get", id)
		if after.Response.Initial != nil || after.Response.FollowUps != 0 || after.Revision != approve.Revision {
			t.Errorf("interaction after the refused responses = %+v", after)
		}

		// Usage errors and missing records.
		wantStderr(t, c.heo(2, "interaction", "respond", id, "--text", "x"), "mode")
		wantStderr(t, c.heo(2, "interaction", "respond", id, "--mode", "message"), "text")
		wantStderr(t, c.heo(2, "interaction", "respond", id, "--mode", "shout", "--text", "x"), "VALIDATION", "want message, ephemeral or update")
		wantStderr(t, c.heo(2, "interaction", "respond", id, "--mode", "message", "--text", "x", "--expires-at", "soon"), "--expires-at")
		wantStderr(t, c.heo(3, "interaction", "respond", "ix_404", "--mode", "message", "--text", "x"), "NOT_FOUND")
		wantStderr(t, c.heo(2, "interaction", "list", "--limit", "0"), "--limit")

		// A viewer reads, and may not answer, preview included.
		c.stop(d)
		c.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
		d = c.start()
		if got := interactionsOf(c); len(got) != 2 {
			t.Errorf("viewer list = %+v", got)
		}
		c.heo(0, "interaction", "get", id)
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			wantDenied(t, c.s.run(nil, append(append([]string{}, respond...), extra...)...), "heo.authz.not_granted")
		}
		c.stop(d)
	})
}

// firstOf is the first of a and b that is set.
func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// TestWidgetInteractionVerbs runs the interaction verbs against the
// maintained adapter that declares respond, the release build's
// native.webwidget: a visitor presses a control and sends a command, heo
// lists and shows them, and answers them in each mode and as a follow-up;
// the visitor's event stream carries each answer. No provider is
// reached, uma being the widget's provider.
func TestWidgetInteractionVerbs(t *testing.T) {
	site := newWidgetSite(t, "")
	d := site.start()
	token, conv := site.visit()
	run := func(exit int, args ...string) result {
		t.Helper()
		r := site.s.run(nil, args...)
		site.s.want(r, exit, args...)
		return r
	}
	asJSON := func(v any, args ...string) {
		t.Helper()
		r := run(0, append(args, "--format", "json")...)
		if err := json.Unmarshal([]byte(r.stdout), v); err != nil {
			t.Fatalf("heo %v: %v\n%s", args, err, r.stdout)
		}
	}
	live := widgetEvents(t, site.route, token, "")
	auth := []string{"Authorization", "Bearer " + token}

	// heo answers the visitor's message; the control sits on the reply.
	var sent replyJSON
	asJSON(&sent, "message", "reply", string(conv.ID), "--text", "Approve the change?")
	reply := next(t, live)
	var replyEv widget.Event
	if err := json.Unmarshal([]byte(reply.data), &replyEv); err != nil || reply.name != widget.EventMessage || replyEv.Message == "" {
		t.Fatalf("reply event = %+v (%v)", reply, err)
	}
	// The event is the adapter's call, not the end of the operation: the
	// widget's message ID binds to the reply when the operation settles, and
	// a press naming it before then finds no control.
	site.s.settleAccepted(d, sent.Operation.ID)

	// The visitor presses the control and types a command.
	press := func(body string) widget.AcceptedInteraction {
		t.Helper()
		code, out := postJSON(t, site.route+"/actions", body, auth...)
		var acc widget.AcceptedInteraction
		if code != http.StatusAccepted || json.Unmarshal([]byte(out), &acc) != nil {
			t.Fatalf("action %s = %d %s", body, code, out)
		}
		return acc
	}
	press(`{"id":"a-1","action":"approve","message":"` + replyEv.Message + `"}`)
	press(`{"id":"a-2","kind":"command","command":"help","arguments":"billing"}`)
	press(`{"id":"a-3","action":"later"}`)
	// A submitted form is recorded too, under its schema.
	formBody := `{"id":"s-1","fields":{"name":"org.example.feedback","version":1,"data":{"rating":5}}}`
	if code, out := postJSON(t, site.route+"/submissions", formBody, auth...); code != http.StatusAccepted {
		t.Fatalf("submission = %d %s", code, out)
	}

	var list pageJSON[model.Interaction]
	asJSON(&list, "interaction", "list", "--conversation", string(conv.ID))
	if len(list.Items) != 4 {
		t.Fatalf("interactions = %+v, want the visitor's four", list.Items)
	}
	byName := map[string]model.Interaction{}
	for _, in := range list.Items {
		byName[firstOf(in.Action, in.Command)] = in
	}
	approve, help, later := byName["approve"], byName["help"], byName["later"]
	if approve.Ref.ID == "" || help.Kind != model.InteractionCommand || help.Arguments != "billing" || later.Ref.ID == "" {
		t.Fatalf("interactions by name = %+v", byName)
	}
	form := byName[""]
	if form.Kind != model.InteractionSubmission || form.Fields.Name != "org.example.feedback" || !strings.Contains(string(form.Fields.Data), "rating") {
		t.Fatalf("submission = %+v", form)
	}
	formText := run(0, "interaction", "get", string(form.Ref.ID)).stdout
	if !strings.Contains(formText, "form") || !strings.Contains(formText, "org.example.feedback v1") {
		t.Errorf("get of a submission does not show the form:\n%s", formText)
	}
	if approve.Message == nil || !approve.RespondUntil.IsZero() {
		t.Fatalf("approve = %+v: want the reply as its control and no respond window (the widget states none)", approve)
	}

	// The preview: what the adapter declares, the audience and the window.
	var plan planJSON
	asJSON(&plan, "interaction", "respond", string(approve.Ref.ID), "--mode", "update", "--text", "Approved.", "--dry-run")
	var capLine string
	_ = json.Unmarshal(plan.Args["capability"], &capLine)
	if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || plan.Effects[0].Target != "interaction:"+string(approve.Ref.ID) ||
		!strings.Contains(capLine, "respond declared by "+string(widget.Type)) || !strings.Contains(capLine, "live availability not checked") ||
		string(plan.Args["audience"]) != `"public"` || string(plan.Args["mode"]) != `"update"` {
		t.Fatalf("preview = %+v capability %q", plan, capLine)
	}
	var wantExpiry string
	_ = json.Unmarshal(plan.Args["expires_at"], &wantExpiry)
	if want := approve.RecordedAt.Add(15 * time.Minute).UTC().Format(time.RFC3339); wantExpiry != want {
		t.Errorf("default expiry = %q, want 15 minutes after it was recorded (%s)", wantExpiry, want)
	}
	ops := (&effectsSite{cliSite: &cliSite{t: t, db: site.db}}).ledger()
	run(0, "interaction", "respond", string(approve.Ref.ID), "--mode", "update", "--text", "Approved.", "--dry-run")
	if (&effectsSite{cliSite: &cliSite{t: t, db: site.db}}).ledger() != ops {
		t.Fatal("--dry-run wrote")
	}

	// An update replaces the control's message on the visitor's page.
	var upd respondJSON
	asJSON(&upd, "interaction", "respond", string(approve.Ref.ID), "--mode", "update", "--text", "Approved.", "--idempotency-key", "ans-approve")
	if upd.Operation.State != model.Queued || !upd.Operation.Durable || upd.Mode != "update" || upd.Visibility != model.Public || upd.FollowUp || upd.Message != "" {
		t.Fatalf("update response = %+v", upd)
	}
	ev := next(t, live)
	var ans widget.Event
	_ = json.Unmarshal([]byte(ev.data), &ans)
	if ev.name != widget.EventInteractionResponse || ans.Mode != model.ResponseModeUpdate || ans.Message != replyEv.Message || ans.FollowUp {
		t.Fatalf("update event = %+v", ev)
	}
	if !strings.Contains(ev.data, "Approved.") {
		t.Errorf("update event does not carry the text: %s", ev.data)
	}
	// A retry under the same key replays; it answers nothing twice.
	var again respondJSON
	asJSON(&again, "interaction", "respond", string(approve.Ref.ID), "--mode", "update", "--text", "Approved.", "--idempotency-key", "ans-approve")
	if !again.Operation.Replayed || again.Operation.ID != upd.Operation.ID {
		t.Fatalf("replayed response = %+v, first %+v", again, upd)
	}
	var got model.Interaction
	asJSON(&got, "interaction", "get", string(approve.Ref.ID))
	if got.Response.Initial == nil || got.Response.Initial.ID != upd.Operation.ID || got.Response.FollowUps != 0 {
		t.Fatalf("interaction after the response = %+v", got.Response)
	}
	// One initial response: a second is a conflict, in a preview and on
	// submission; a follow-up is another, which needs no claim.
	wantStderr(t, run(4, "interaction", "respond", string(approve.Ref.ID), "--mode", "message", "--text", "Again.", "--dry-run"),
		"CONFLICT", "already has an initial response")
	wantStderr(t, run(4, "interaction", "respond", string(approve.Ref.ID), "--mode", "message", "--text", "Again.", "--idempotency-key", "ans-again"), "CONFLICT")
	var fu respondJSON
	asJSON(&fu, "interaction", "respond", string(approve.Ref.ID), "--mode", "ephemeral", "--text", "Only you.", "--follow-up", "--idempotency-key", "ans-fu")
	if !fu.FollowUp || fu.Visibility != model.Restricted || fu.Mode != "ephemeral" {
		t.Fatalf("follow-up = %+v", fu)
	}
	ev = next(t, live)
	ans = widget.Event{}
	_ = json.Unmarshal([]byte(ev.data), &ans)
	if ev.name != widget.EventInteractionResponse || ans.Mode != model.ResponseModeEphemeral || !ans.FollowUp {
		t.Fatalf("follow-up event = %+v", ev)
	}
	asJSON(&got, "interaction", "get", string(approve.Ref.ID))
	if got.Response.FollowUps != 1 {
		t.Fatalf("follow-ups = %d, want 1", got.Response.FollowUps)
	}

	// A message answer is a new public message, and uma keeps it.
	var msg respondJSON
	asJSON(&msg, "interaction", "respond", string(help.Ref.ID), "--mode", "message", "--text", "Billing is on the Plans page.", "--idempotency-key", "ans-help")
	if msg.Message == "" || msg.Visibility != model.Public {
		t.Fatalf("message response = %+v: want the message uma committed", msg)
	}
	ev = next(t, live)
	if ev.name != widget.EventInteractionResponse || !strings.Contains(ev.data, "Billing is on the Plans page.") {
		t.Fatalf("message event = %+v", ev)
	}
	var kept model.Message
	asJSON(&kept, "message", "get", string(msg.Message))
	if kept.Ref.ID != msg.Message || desk.Text(kept.Content) != "Billing is on the Plans page." {
		t.Fatalf("the committed message = %+v", kept)
	}

	// An update needs the control's message: none, refused in a preview.
	wantStderr(t, run(2, "interaction", "respond", string(later.Ref.ID), "--mode", "update", "--text", "x", "--dry-run"), "VALIDATION", "update")

	// The window: the default is derived from the record; a named expiry
	// in the past is refused, one beyond it is fine where the provider
	// states no window; a bad time is a usage error.
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	wantStderr(t, run(1, "interaction", "respond", string(later.Ref.ID), "--mode", "ephemeral", "--text", "x", "--expires-at", past, "--dry-run"), "DEADLINE_EXPIRED")
	var far respondJSON
	farOut := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	asJSON(&far, "interaction", "respond", string(later.Ref.ID), "--mode", "ephemeral", "--text", "Later it is.", "--expires-at", farOut, "--idempotency-key", "ans-later")
	if far.ExpiresAt.Format(time.RFC3339) != farOut {
		t.Errorf("expiry = %v, want %s", far.ExpiresAt, farOut)
	}
	next(t, live)

	// What the provider was told is what the log says: every answer is an
	// event, and the daemon's own interaction.updated follows each.
	var evs pageJSON[model.Event]
	asJSON(&evs, "event", "list", "--kind", "interaction.received,interaction.updated")
	counts := map[string]int{}
	for _, e := range evs.Items {
		counts[e.Kind]++
	}
	if counts[model.EventInteractionReceived] != 4 || counts[model.EventInteractionUpdated] < 4 {
		t.Errorf("interaction events = %v, want 4 received and an update for each answer", counts)
	}
	if code := d.term(); code != 0 {
		t.Fatalf("SIGTERM exit %d:\n%s", code, d.log())
	}
}
