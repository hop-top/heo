package desk_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/trustcfg"
	"hop.top/uma/model"
)

// stateCases are the provider state changes of heo's verbs, with the
// verb family and class the hold point offers each under.
var stateCases = []struct {
	name         string
	ch           desk.StateChange
	verb, class  string
	threshold    int
	needsLabels  bool
	asked        string
	trustClassOf string
}{
	{"archive", desk.StateChange{Verb: model.StateVerbArchive}, "state", "archive", 3, false, "archive", "archive"},
	{"unarchive", desk.StateChange{Verb: model.StateVerbUnarchive}, "state", "unarchive", trustcfg.DefaultThreshold, false, "unarchive", "state:unarchive"},
	{"read", desk.StateChange{Verb: model.StateVerbRead}, "state", "read", 3, false, "read", "read"},
	{"unread", desk.StateChange{Verb: model.StateVerbUnread}, "state", "unread", 3, false, "unread", "unread"},
	{"star", desk.StateChange{Verb: model.StateVerbFlag, Kind: model.FlagStar}, "state", "star", 3, false, "flag star", "star"},
	{"unstar", desk.StateChange{Verb: model.StateVerbUnflag, Kind: model.FlagStar}, "state", "unstar", 3, false, "unflag star", "unstar"},
	{"important", desk.StateChange{Verb: model.StateVerbFlag, Kind: model.FlagImportant}, "state", "important", trustcfg.DefaultThreshold, false, "flag important", "state:important"},
	{"trash", desk.StateChange{Verb: model.StateVerbTrash}, "state", "trash", 5, false, "trash", "trash"},
	{"untrash", desk.StateChange{Verb: model.StateVerbUntrash}, "state", "untrash", trustcfg.DefaultThreshold, false, "untrash", "state:untrash"},
	{"move to the trash role is trash", desk.StateChange{Verb: model.StateVerbMove, Destination: model.MoveDestination{Role: model.RoleTrash}},
		"state", "trash", 5, false, "move to the trash role", "trash"},
	{"move to the junk role is spam", desk.StateChange{Verb: model.StateVerbMove, Destination: model.MoveDestination{Role: model.RoleJunk}},
		"state", "spam", 5, false, "move to the junk role", "spam"},
	{"move to a role", desk.StateChange{Verb: model.StateVerbMove, Destination: model.MoveDestination{Role: model.RoleInbox}},
		"move", "inbox", 3, false, "move to the inbox role", "move_to"},
	{"move to an ID", desk.StateChange{Verb: model.StateVerbMove, Destination: model.MoveDestination{ID: "Label_9"}},
		"move", "Label_9", 3, false, "move to Label_9", "move_to"},
	{"move by labels", desk.StateChange{Verb: model.StateVerbMove, Add: []string{"Label_12"}, Remove: []string{"INBOX"}},
		"move", "+Label_12,-INBOX", 3, true, "move add Label_12; remove INBOX", "move_to"},
}

// TestStateChangeHoldClasses: each change is offered to the hold point
// under the verb and class the trust ladder's table names, so the
// ladder's thresholds apply to it; a move into the trash or junk role
// is the trash or spam it is, never a cheaper class.
func TestStateChangeHoldClasses(t *testing.T) {
	for _, tc := range stateCases {
		verb, class := tc.ch.HoldClass()
		if verb != tc.verb || class != tc.class {
			t.Errorf("%s: hold class = %s:%s, want %s:%s", tc.name, verb, class, tc.verb, tc.class)
		}
		if got := trustcfg.Class(verb, class); got != tc.trustClassOf {
			t.Errorf("%s: ladder class = %q, want %q", tc.name, got, tc.trustClassOf)
		}
		if got := trustcfg.Defaults().Threshold(trustcfg.Class(verb, class)); got != tc.threshold {
			t.Errorf("%s: threshold = %d, want %d", tc.name, got, tc.threshold)
		}
		if got := tc.ch.Asked(); got != tc.asked {
			t.Errorf("%s: asked = %q, want %q", tc.name, got, tc.asked)
		}
	}
}

// TestStateVerbsNameTheirCapability: one capability per verb pair, and
// the verb tables name each command.
func TestStateVerbsNameTheirCapability(t *testing.T) {
	want := map[model.StateVerb]model.Capability{
		model.StateVerbArchive: model.CapabilityArchive, model.StateVerbUnarchive: model.CapabilityArchive,
		model.StateVerbMove: model.CapabilityMove, model.StateVerbRead: model.CapabilityReadState, model.StateVerbUnread: model.CapabilityReadState,
		model.StateVerbFlag: model.CapabilityFlag, model.StateVerbUnflag: model.CapabilityFlag,
		model.StateVerbTrash: model.CapabilityTrash, model.StateVerbUntrash: model.CapabilityTrash,
	}
	if len(desk.StateVerbs) != len(want) {
		t.Fatalf("%d verbs, want %d", len(desk.StateVerbs), len(want))
	}
	for _, v := range desk.StateVerbs {
		if c, _ := v.Capability(); c != want[v] || desk.StateVerbName(v) != "conversation "+string(v) {
			t.Errorf("%s: capability %s, name %q", v, c, desk.StateVerbName(v))
		}
	}
}

// TestStateChangeUsageRules: the rules that need no record or adapter.
func TestStateChangeUsageRules(t *testing.T) {
	for name, tc := range map[string]struct {
		ch   desk.StateChange
		want string
	}{
		"not a verb":           {desk.StateChange{Verb: "delete"}, "not a provider state verb"},
		"flag without a kind":  {desk.StateChange{Verb: model.StateVerbFlag}, "want star or important"},
		"flag of another kind": {desk.StateChange{Verb: model.StateVerbUnflag, Kind: "pin"}, "want star or important"},
		"move to nothing":      {desk.StateChange{Verb: model.StateVerbMove}, "name where to move it"},
		"id and role":          {desk.StateChange{Verb: model.StateVerbMove, Destination: model.MoveDestination{ID: "x", Role: model.RoleInbox}}, "not both"},
		"destination and labels": {desk.StateChange{Verb: model.StateVerbMove, Destination: model.MoveDestination{ID: "x"}, Add: []string{"a"}},
			"exclusive with --add and --remove"},
		"unknown role": {desk.StateChange{Verb: model.StateVerbMove, Destination: model.MoveDestination{Role: "spam"}}, "want inbox, archive, trash or junk"},
	} {
		err := desk.CheckStateChange(tc.ch)
		if !model.IsCode(err, model.Validation) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want a validation error containing %q", name, err, tc.want)
		}
	}
	for _, ok := range []desk.StateChange{
		{Verb: model.StateVerbArchive}, {Verb: model.StateVerbFlag, Kind: model.FlagImportant},
		{Verb: model.StateVerbMove, Destination: model.MoveDestination{Role: model.RoleJunk}},
		{Verb: model.StateVerbMove, Remove: []string{"INBOX"}},
	} {
		if err := desk.CheckStateChange(ok); err != nil {
			t.Errorf("%+v refused: %v", ok, err)
		}
	}
}

// TestStateQueuesProviderStateOperations: a state change is uma's own
// provider state operation on the conversation, authorized by the scope
// the channel reports (account, for both mail adapters); planning writes
// nothing; submitting queues one operation and changes nothing of uma's
// conversation (status, labels, revision, read marker); a retry under the
// same key replays and queues nothing more.
func TestStateQueuesProviderStateOperations(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm, ms := w.gmailThread(peerMessage), w.outlookThread("AAMk-1")
	before := w.queued()
	n := 0
	for _, conv := range []struct {
		ref     model.ConversationRef
		folders bool
	}{{gm, false}, {ms, true}} {
		for _, tc := range stateCases {
			if tc.needsLabels && conv.folders {
				continue
			}
			n++
			key := model.OperationID(fmt.Sprintf("st-%d", n))
			p, err := d.PlanState(ctx, string(conv.ref.ID), tc.ch, "", key, channels.Submit)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if p.Action != model.ActionConversationStateAccount || p.Declaration != nil || p.Limits != nil || p.Conversation.Ref != conv.ref {
				t.Fatalf("%s: plan = %+v", tc.name, p)
			}
			if got := w.queued(); got != before {
				t.Fatalf("%s: planning queued %d provider work items", tc.name, got-before)
			}
			conv0 := w.conversation(conv.ref)
			c, acc, err := d.SubmitState(ctx, p)
			if err != nil || acc.Operation.State != model.Queued || acc.Replayed {
				t.Fatalf("%s: submit = %+v, %v", tc.name, acc, err)
			}
			if rec := w.record(acc.Operation.Ref); rec.Key.Kind != model.ActionConversationStateAccount || rec.Key.Target != conv.ref.Ref() {
				t.Fatalf("%s: operation key = %+v, want a provider state change on the conversation", tc.name, rec.Key)
			}
			if c.Revision != conv0.Revision || c.Status != conv0.Status || !reflect.DeepEqual(c.Labels, conv0.Labels) ||
				!reflect.DeepEqual(c.ProviderState, conv0.ProviderState) {
				t.Errorf("%s: the conversation changed before the provider applied it: %+v", tc.name, c)
			}
			before++
			if got := w.queued(); got != before {
				t.Fatalf("%s: queued %d provider work items, want %d", tc.name, got, before)
			}
			if _, again, err := d.SubmitState(ctx, p); err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
				t.Fatalf("%s: retry = %+v, %v, want %v replayed", tc.name, again, err, acc.Operation.Ref)
			}
			if got := w.queued(); got != before {
				t.Fatalf("%s: a replay queued work: %d", tc.name, got-before)
			}
		}
	}
}

// TestStatePreviewReadsTheChannelsLimits: a preview checks that the
// adapter declares the capability and reads what the channel reports of
// it now: the scope that authorizes the change, and the shape, roles and
// kinds it checks the change against. Gmail files by label, Graph in one
// folder, so a label move is Gmail's alone.
func TestStatePreviewReadsTheChannelsLimits(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm, ms := w.gmailThread(peerMessage), w.outlookThread("AAMk-1")

	p, err := d.PlanState(ctx, string(gm.ID), desk.StateChange{Verb: model.StateVerbArchive}, "", "pv-1", channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	if p.Declaration == nil || p.Declaration.Capability != model.CapabilityArchive || p.Limits == nil ||
		p.Limits.Scope != model.ScopeAccount || p.Action != model.ActionConversationStateAccount {
		t.Fatalf("preview = %+v", p)
	}
	p, err = d.PlanState(ctx, string(gm.ID), desk.StateChange{Verb: model.StateVerbMove, Add: []string{"Label_1"}}, "", "pv-2", channels.Preview)
	if err != nil || p.Limits == nil || p.Limits.Shape != model.MoveShapeLabels || !strings.Contains(strings.Join(p.Limits.Roles, ","), model.RoleJunk) {
		t.Fatalf("gmail label move = %+v, %v", p, err)
	}
	_, err = d.PlanState(ctx, string(ms.ID), desk.StateChange{Verb: model.StateVerbMove, Add: []string{"Cat"}}, "", "pv-3", channels.Preview)
	wantCode(t, err, model.Unsupported, "one folder")
	// A submission leaves the live checks to uma.
	if _, err := d.PlanState(ctx, string(ms.ID), desk.StateChange{Verb: model.StateVerbMove, Add: []string{"Cat"}}, "", "pv-3", channels.Submit); err != nil {
		t.Fatalf("submission planned with a label move on a folder channel: %v", err)
	}
	// The preview wrote nothing.
	if n := w.queued(); n != 0 {
		t.Fatalf("previews queued %d provider work items", n)
	}
}

// TestStateRefusals: usage errors, an unknown conversation, a stale
// revision and an adapter that declares no state capability are refused
// before anything is written, in a preview and on submission alike.
func TestStateRefusals(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm := w.gmailThread(peerMessage)
	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		_, err := d.PlanState(ctx, string(gm.ID), desk.StateChange{Verb: model.StateVerbFlag}, "", "rf-1", mode)
		wantCode(t, err, model.Validation, "want star or important")
		_, err = d.PlanState(ctx, "no-such", desk.StateChange{Verb: model.StateVerbArchive}, "", "rf-1", mode)
		wantCode(t, err, model.NotFound, "")
		// Zendesk declares none of the state capabilities.
		for _, v := range desk.StateVerbs {
			ch := desk.StateChange{Verb: v, Kind: model.FlagStar, Destination: model.MoveDestination{Role: model.RoleInbox}}
			_, err = d.PlanState(ctx, string(ticketConv), ch, "", "rf-2", mode)
			wantCode(t, err, model.Unsupported, "does not declare")
			if !strings.Contains(err.Error(), "`"+desk.StateVerbName(v)+"`") {
				t.Errorf("%s refusal does not name the verb: %v", v, err)
			}
		}
		_, err = d.PlanState(ctx, string(gm.ID), desk.StateChange{Verb: model.StateVerbArchive}, "stale", "rf-3", mode)
		if mode == channels.Preview {
			wantCode(t, err, model.Conflict, "changed since it was read")
		} else if err != nil {
			t.Fatalf("a stale revision is uma's to refuse on submission: %v", err)
		}
	}
	p, err := d.PlanState(ctx, string(gm.ID), desk.StateChange{Verb: model.StateVerbArchive}, "stale", "rf-4", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = d.SubmitState(ctx, p)
	wantCode(t, err, model.Conflict, "changed since it was read")
	if n := w.queued(); n != 0 {
		t.Fatalf("a refused change queued %d provider work items", n)
	}
	// A viewer reads the conversation and may not change its provider state.
	_, _, err = func() (model.Conversation, model.Acceptance, error) {
		v := w.asKnowing(viewer)
		p, err := v.PlanState(ctx, string(gm.ID), desk.StateChange{Verb: model.StateVerbArchive}, "", "rf-5", channels.Submit)
		if err != nil {
			return model.Conversation{}, model.Acceptance{}, err
		}
		return v.SubmitState(ctx, p)
	}()
	wantCode(t, err, model.Unauthorized, "")
}

// TestHeldStateChanges: a state change proposed by a plugin actor is
// offered to the hold point under its trust class, stored held with no
// provider work queued; approve queues it exactly once, reject queues
// nothing. It is probed against uma's store for every verb.
func TestHeldStateChanges(t *testing.T) {
	for _, tc := range stateCases {
		t.Run(tc.name, func(t *testing.T) {
			w := newTicketWorld(t)
			var asked []desk.HoldQuery
			plug, _ := holding(w.asKnowing(plugin), recordPolicy{&asked})
			gm := w.gmailThread(peerMessage)
			before := w.queued()
			propose := func(s desk.Service, key string) (model.Acceptance, error) {
				p, err := s.PlanState(t.Context(), string(gm.ID), tc.ch, "", model.OperationID(key), channels.Submit)
				if err != nil {
					return model.Acceptance{}, err
				}
				verb, class := tc.ch.HoldClass()
				h, err := s.Hold(t.Context(), &p.Command, desk.HoldInput{Action: p.Action, Verb: verb, Class: class,
					Channel: string(gm.Inbox.ID), Target: gm.Ref(), Preview: "p"})
				if err != nil {
					return model.Acceptance{}, err
				}
				_, acc, err := s.SubmitState(t.Context(), p)
				return h.Settle(t.Context(), acc, err)
			}
			acc, err := propose(plug, "hs-rj")
			if err != nil || acc.Operation.State != model.Held || w.queued() != before {
				t.Fatalf("hold = %+v, %v, queued %d", acc, err, w.queued()-before)
			}
			if len(asked) != 1 || asked[0].Verb != tc.verb || asked[0].Class != tc.class ||
				asked[0].Action != model.ActionConversationStateAccount || asked[0].Channel != string(gm.Inbox.ID) {
				t.Fatalf("hold query = %+v, want %s:%s as a state_account write", asked, tc.verb, tc.class)
			}
			dec := w.asKnowing(agent)
			if r, err := dec.DecideHeld(t.Context(), desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideReject}); err != nil || r.Status != string(model.Rejected) {
				t.Fatalf("reject = %+v, %v", r, err)
			}
			if w.queued() != before {
				t.Fatalf("a rejected change queued %d", w.queued()-before)
			}
			acc, err = propose(plug, "hs-ap")
			if err != nil || acc.Operation.State != model.Held {
				t.Fatalf("hold = %+v, %v", acc, err)
			}
			if r, err := dec.DecideHeld(t.Context(), desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideApprove}); err != nil || r.Replayed {
				t.Fatalf("approve = %+v, %v", r, err)
			}
			if w.queued() != before+1 {
				t.Fatalf("approved change queued %d, want 1", w.queued()-before)
			}
			if r, err := dec.DecideHeld(t.Context(), desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideApprove}); err != nil || !r.Replayed || w.queued() != before+1 {
				t.Fatalf("second approve = %+v, %v, queued %d", r, err, w.queued()-before)
			}
		})
	}
}

// TestHoldableStateActions pins the table: both scopes a state change is
// authorized as are holdable.
func TestHoldableStateActions(t *testing.T) {
	for _, a := range []string{model.ActionConversationStateAccount, model.ActionConversationStateShared} {
		if !desk.CanHold(a) {
			t.Errorf("%s is not holdable", a)
		}
	}
}
