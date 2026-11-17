package e2e

import (
	"slices"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/model"
)

type conversationStateJSON struct {
	Conversation model.Conversation
	Verb         string
	Change       string
	Scope        string
	Operation    opJSON
	OperationKey string `json:"operation_key"`
	Notes        []string
}

// TestConversationStateLifecycle drives the e2e build on Postgres through
// the nine provider state verbs on fake providers that declare the
// conversation state capabilities: a mailbox's (account scope, label
// moves, star and important, every role) and a folder-shaped shared
// provider's (archive, move and star only). A preview names the external
// change, the scope the channel reports and what it checked, and writes
// nothing; each verb queues durably under its key, replays without a
// second request, and reaches the provider with exactly what was asked;
// uma's own status and labels are never touched. A flag kind, a role or a
// label move the channel does not have, a capability the adapter does not
// declare, a usage error, an unknown conversation and a viewer are all
// refused before anything is written, with the exit codes the grammar
// gives.
func TestConversationStateLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		s := newEffectSite(t, noRateLimit)
		st := s.mail(s.st, "stin", "st-1", "ev-st", "gmail-msg-s")
		sid := string(st.Ref.ID)

		// The preview.
		var plan planJSON
		s.json(&plan, "conversation", "archive", sid, "--dry-run")
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || plan.Effects[0].Target != "provider_state:"+sid {
			t.Fatalf("preview effects = %+v", plan.Effects)
		}
		for _, want := range []string{"external", "archive", "the provider's own state"} {
			if !strings.Contains(plan.Effects[0].Detail, want) {
				t.Errorf("effect detail lacks %q: %s", want, plan.Effects[0].Detail)
			}
		}
		if string(plan.Args["scope"]) != `"account"` || string(plan.Args["change"]) != `"archive"` {
			t.Errorf("preview args = %v", plan.Args)
		}
		if line := s.capabilityLine(plan); !strings.Contains(line, "archive declared by "+string(adapterstest.StateType)) {
			t.Errorf("capability line = %q", line)
		}
		start := s.ledger()
		s.heo(0, "conversation", "move", sid, "--role", "junk", "--dry-run")
		s.heo(0, "conversation", "flag", sid, "--kind", "important", "--dry-run")
		wantStderr(t, s.heo(2, "conversation", "flag", sid, "--kind", "pin"), "want star or important")
		wantStderr(t, s.heo(2, "conversation", "move", sid, "--to", "Label_1", "--add", "Label_2"), "exclusive with --add and --remove")
		wantStderr(t, s.heo(2, "conversation", "archive", sid, "--url", "https://evil.example/"), "unknown flag")
		if n := s.ledger(); n != start || len(s.applied()) != 0 {
			t.Fatalf("--dry-run or a usage error wrote: operations %d, was %d; provider saw %v", n, start, s.applied())
		}

		// The channel's live limits are on the conversation.
		var cv struct {
			Verbs []struct {
				Verb, State string
				Limits      *model.StateLimits
			}
		}
		s.json(&cv, "conversation", "get", sid)
		var gotMove, gotFlag bool
		for _, v := range cv.Verbs {
			switch v.Verb {
			case "conversation move":
				gotMove = v.State == "available" && v.Limits != nil && v.Limits.Scope == model.ScopeAccount &&
					v.Limits.Shape == model.MoveShapeLabels && slices.Contains(v.Limits.Roles, model.RoleJunk)
			case "conversation flag":
				gotFlag = v.State == "available" && v.Limits != nil && slices.Equal(v.Limits.Kinds, []string{model.FlagStar, model.FlagImportant})
			}
		}
		if !gotMove || !gotFlag {
			t.Errorf("conversation get verbs = %+v, want move and flag available with the channel's limits", cv.Verbs)
		}

		// Each verb queues, replays and reaches the provider.
		route := peerMail
		type step struct {
			args []string
			key  string
			line string
		}
		steps := []step{
			{[]string{"archive"}, "ss-1", "state archive " + route},
			{[]string{"unarchive"}, "ss-2", "state unarchive " + route},
			{[]string{"move", "--role", "junk"}, "ss-3", "state move " + route + " role=junk"},
			{[]string{"move", "--add", "Label_12", "--remove", "INBOX"}, "ss-4", "state move " + route + " +Label_12 -INBOX"},
			{[]string{"read"}, "ss-5", "state read " + route},
			{[]string{"unread"}, "ss-6", "state unread " + route},
			{[]string{"flag"}, "ss-7", "state flag " + route + " kind=star"},
			{[]string{"unflag", "--kind", "important"}, "ss-8", "state unflag " + route + " kind=important"},
			{[]string{"trash"}, "ss-9", "state trash " + route},
			{[]string{"untrash"}, "ss-10", "state untrash " + route},
		}
		var want []string
		for i, step := range steps {
			args := append([]string{"conversation", step.args[0], sid}, step.args[1:]...)
			var out, again conversationStateJSON
			s.json(&out, append(args, "--idempotency-key", step.key)...)
			if out.Operation.State != model.Queued || !out.Operation.Durable || out.Operation.Replayed || out.OperationKey != step.key ||
				out.Verb != step.args[0] || out.Conversation.Ref != st.Ref {
				t.Fatalf("%s = %+v", step.args[0], out)
			}
			s.json(&again, append(args, "--idempotency-key", step.key)...)
			if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
				t.Fatalf("replayed %s = %+v, first %+v", step.args[0], again, out)
			}
			if d := s.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered); d.State == model.Failed {
				t.Fatalf("%s failed: %+v", step.args[0], d.Failure)
			}
			want = append(want, step.line)
			s.wantApplied(i + 1)
		}
		got := s.applied()
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("provider was asked %q, want %q", got, want)
		}
		if op := s.operation("--key", "ss-1"); op.Kind != model.ActionConversationStateAccount {
			t.Errorf("operation get --key ss-1 kind = %q, want a provider state change of account scope", op.Kind)
		}
		var after model.Conversation
		s.json(&after, "conversation", "get", sid)
		if after.Status != st.Status || !slices.Equal(after.Labels, st.Labels) {
			t.Errorf("provider state verbs changed uma's own status or labels: %+v, was %+v", after, st)
		}

		// The shared, folder-shaped provider: the scope is shared, and what
		// its limits do not allow is refused at the preview.
		sf := s.mail(s.sf, "sfin", "sf-1", "ev-sf", "gmail-msg-f")
		fid := string(sf.Ref.ID)
		s.json(&plan, "conversation", "archive", fid, "--dry-run")
		if string(plan.Args["scope"]) != `"shared"` || !strings.Contains(plan.Effects[0].Detail, "shared") {
			t.Errorf("shared preview = %v, %+v", plan.Args, plan.Effects)
		}
		before := s.ledger()
		applied := len(s.applied())
		wantStderr(t, s.heo(2, "conversation", "flag", fid, "--kind", "important", "--dry-run"), "UNSUPPORTED", "has no important flag")
		wantStderr(t, s.heo(2, "conversation", "move", fid, "--role", "junk", "--dry-run"), "UNSUPPORTED", "does not resolve the junk role")
		wantStderr(t, s.heo(2, "conversation", "move", fid, "--add", "Label_1", "--dry-run"), "UNSUPPORTED", "one folder")
		for _, verb := range []string{"read", "unread", "trash", "untrash"} {
			wantStderr(t, s.heo(2, "conversation", verb, fid, "--dry-run"), "UNSUPPORTED", "does not declare")
			wantStderr(t, s.heo(2, "conversation", verb, fid, "--idempotency-key", "x-"+verb), "UNSUPPORTED")
		}
		if n := s.ledger(); n != before || len(s.applied()) != applied {
			t.Errorf("a refused change wrote: operations %d, was %d; provider %v", n, before, s.applied())
		}
		var shared conversationStateJSON
		s.json(&shared, "conversation", "archive", fid, "--idempotency-key", "sf-1")
		if shared.Operation.State != model.Queued {
			t.Fatalf("shared archive = %+v", shared)
		}
		s.wantApplied(applied + 1)
		if op := s.operation("--key", "sf-1"); op.Kind != model.ActionConversationStateShared {
			t.Errorf("operation get --key sf-1 kind = %q, want a provider state change of shared scope", op.Kind)
		}

		// An adapter that declares none, and an unknown conversation.
		before, applied = s.ledger(), len(s.applied())
		pl := s.mail(s.plain, "plmail", "pl-1", "ev-pl", "plain-msg-1")
		for _, verb := range []string{"archive", "unarchive", "move", "read", "unread", "flag", "unflag", "trash", "untrash"} {
			args := []string{"conversation", verb, string(pl.Ref.ID)}
			if verb == "move" {
				args = append(args, "--role", "inbox")
			}
			wantStderr(t, s.heo(2, append(args, "--dry-run")...), "UNSUPPORTED", "does not declare")
			wantStderr(t, s.heo(2, append(args, "--idempotency-key", "p-"+verb)...), "UNSUPPORTED")
		}
		wantStderr(t, s.heo(3, "conversation", "archive", "00000ad0-0000-7000-8000-0000000009ff"), "NOT_FOUND")
		if n := s.ledger(); n != before || len(s.applied()) != applied {
			t.Errorf("a refused change wrote: operations %d, was %d; provider %v", n, before, s.applied())
		}

		// A viewer is denied, in a preview and for real.
		s.stop(s.d)
		s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
		s.d = s.start()
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			wantDenied(t, s.s.run(nil, append([]string{"conversation", "archive", sid}, extra...)...), "heo.authz.not_granted")
		}
		if len(s.applied()) != applied {
			t.Errorf("provider saw %v", s.applied())
		}
		s.stop(s.d)
	})
}
