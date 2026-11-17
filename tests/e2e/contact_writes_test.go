package e2e

import (
	"slices"
	"strings"
	"testing"

	"hop.top/uma/model"
)

type contactChangeJSON struct {
	Contact      model.Contact
	Operation    opJSON
	OperationKey string `json:"operation_key"`
	Notes        []string
}

type contactMergeJSON struct {
	From         model.Contact
	Into         model.Contact
	Moved        []model.IdentityLink
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

func (s *deskSite) contact(id string) model.Contact {
	s.t.Helper()
	var v contactViewJSON
	s.json(&v, "contact", "get", id)
	return v.Contact
}

// planList reads a plan argument that is a comma-separated list.
func planList(t *testing.T, p planJSON, key string) []string {
	t.Helper()
	raw, ok := p.Args[key]
	if !ok {
		t.Fatalf("plan has no %s: %+v", key, p.Args)
	}
	s := strings.Trim(string(raw), `"`)
	if s == "" {
		return nil
	}
	return strings.Split(s, ", ")
}

// TestContactCreateUpdateCommands drives the release binary on Postgres:
// a contact is created and edited at the revision the caller read. A
// preview writes nothing, a stale revision is a conflict that writes
// nothing, a repeated key replays, and a viewer is refused.
func TestContactCreateUpdateCommands(t *testing.T) {
	s := newDeskSite(t)
	var before pageJSON[model.Contact]
	s.json(&before, "contact", "list")

	// Shape.
	wantStderr(t, s.heo(2, "contact", "create"), "USAGE", "name")
	wantStderr(t, s.heo(2, "contact", "create", "--name", "Zed", "--kind", "robot"), "VALIDATION", "kind")
	wantStderr(t, s.heo(2, "contact", "create", "--name", "Zed", "--email", "not an address"), "VALIDATION", "email")

	// Preview: the contact as it would be created, normalized; nothing written.
	create := []string{"contact", "create", "--name", "Grace Hopper", "--email", "grace@Example.com", "--phone", "+1 202-555-0100"}
	var plan planJSON
	s.json(&plan, append(create, "--dry-run")...)
	if len(plan.Effects) != 1 || plan.Effects[0].Kind != "create" || string(plan.Args["name"]) != `"Grace Hopper"` ||
		!strings.Contains(string(plan.Args["attributes"]), "email grace@example.com") ||
		!strings.Contains(string(plan.Args["attributes"]), "phone +12025550100") {
		t.Fatalf("create preview = %+v", plan)
	}
	var after pageJSON[model.Contact]
	if s.json(&after, "contact", "list"); len(after.Items) != len(before.Items) {
		t.Fatalf("create --dry-run wrote: %+v", after.Items)
	}

	var made, again contactChangeJSON
	s.json(&made, append(create, "--idempotency-key", "mk-1")...)
	c := made.Contact
	if c.DisplayName != "Grace Hopper" || c.Kind != model.ContactKindPerson || c.Revision == "" || c.MergedInto != nil ||
		len(c.Attributes) != 2 || made.Operation.State != model.Committed || made.Operation.Replayed || made.OperationKey != "mk-1" {
		t.Fatalf("create = %+v", made)
	}
	s.json(&again, append(create, "--idempotency-key", "mk-1")...)
	if !again.Operation.Replayed || again.Operation.ID != made.Operation.ID || again.Contact.Ref != c.Ref {
		t.Fatalf("replayed create = %+v", again)
	}
	if s.json(&after, "contact", "list"); len(after.Items) != len(before.Items)+1 {
		t.Fatalf("contacts after replay = %d, want %d", len(after.Items), len(before.Items)+1)
	}
	var cv contactViewJSON
	if s.json(&cv, "contact", "get", string(c.Ref.ID)); len(cv.Identities) != 0 {
		t.Fatalf("a created contact holds identities: %+v", cv.Identities)
	}
	r := s.heo(0, "contact", "get", string(c.Ref.ID))
	for _, want := range [][2]string{{"name", "Grace"}, {"email", "grace@example.com"}, {"phone", "+12025550100"}, {"revision", string(c.Revision)}} {
		if !strings.Contains(r.stdout, want[0]) || !strings.Contains(r.stdout, want[1]) {
			t.Errorf("contact get lacks %v:\n%s", want, r.stdout)
		}
	}

	// Update: the revision is required, and a stale one writes nothing.
	id, rev := string(c.Ref.ID), string(c.Revision)
	up := []string{"contact", "update", id}
	wantStderr(t, s.heo(2, append(up, "--name", "Grace B. Hopper")...), "USAGE", "revision")
	wantStderr(t, s.heo(4, append(up, "--name", "Grace B. Hopper", "--revision", "stale")...), "CONFLICT", "is at revision "+rev)
	wantStderr(t, s.heo(2, append(up, "--revision", rev)...), "VALIDATION", "nothing to change")
	s.json(&plan, append(up, "--name", "Grace B. Hopper", "--revision", rev, "--dry-run")...)
	if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || plan.Effects[0].Target != "contact:"+id ||
		string(plan.Args["expected_revision"]) != `"`+rev+`"` || string(plan.Args["change"]) != `"display_name"` {
		t.Fatalf("update preview = %+v", plan)
	}
	if got := s.contact(id); got.Revision != c.Revision || got.DisplayName != "Grace Hopper" {
		t.Fatalf("update --dry-run wrote: %+v", got)
	}
	var upd, rep contactChangeJSON
	s.json(&upd, append(up, "--name", "Grace B. Hopper", "--revision", rev, "--idempotency-key", "up-1")...)
	if upd.Contact.DisplayName != "Grace B. Hopper" || upd.Contact.Revision == c.Revision || len(upd.Contact.Attributes) != 2 ||
		upd.Operation.State != model.Committed || upd.OperationKey != "up-1" {
		t.Fatalf("update = %+v", upd)
	}
	// The same key replays, though the revision has moved on.
	s.json(&rep, append(up, "--name", "Grace B. Hopper", "--revision", rev, "--idempotency-key", "up-1")...)
	if !rep.Operation.Replayed || rep.Operation.ID != upd.Operation.ID || rep.Contact.Revision != upd.Contact.Revision {
		t.Fatalf("replayed update = %+v", rep)
	}
	// An attribute kind given replaces that kind alone; empty clears it.
	cur := upd.Contact
	s.json(&upd, append(up, "--email", "hopper@navy.example", "--revision", string(cur.Revision))...)
	if got := upd.Contact.Attributes; len(got) != 2 || got[0].Value != "hopper@navy.example" || got[1].Value != "+12025550100" {
		t.Fatalf("email replaced = %+v", got)
	}
	s.json(&upd, append(up, "--phone=", "--revision", string(upd.Contact.Revision))...)
	if got := upd.Contact.Attributes; len(got) != 1 || got[0].Kind != model.AttributeEmail {
		t.Fatalf("phone cleared = %+v", got)
	}
	// Setting what is set commits nothing and keeps the revision.
	cur = upd.Contact
	s.json(&upd, append(up, "--name", cur.DisplayName, "--revision", string(cur.Revision))...)
	if upd.Contact.Revision != cur.Revision || len(upd.Notes) != 1 {
		t.Fatalf("no-op update = %+v", upd)
	}
	s.stop(s.d)

	// A viewer reads contacts and creates and edits none.
	s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
	s.d = s.start()
	wantStderr(t, s.heo(5, "contact", "create", "--name", "Mallory"), "UNAUTHORIZED", "heo.authz.not_granted")
	wantStderr(t, s.heo(5, append(up, "--name", "Mallory", "--revision", string(cur.Revision))...), "UNAUTHORIZED", "heo.authz.not_granted")
	if got := s.contact(id); got.Revision != cur.Revision {
		t.Fatalf("viewer wrote: %+v", got)
	}
	s.stop(s.d)
}

// TestContactMergeCommand drives the release binary on Postgres through
// two real widget visitors: the preview is uma's and writes nothing;
// the merge needs the confirmation, both revisions and a reason; what
// it moves is what the preview listed; the contact merged reads as a
// redirect and the identity's history shows the move; a stale revision,
// a repeat under another key and a merge into a merged contact are
// conflicts; the same key replays without moving twice; a viewer is
// refused.
func TestContactMergeCommand(t *testing.T) {
	// Kit rate-limits destructive commands per caller (a burst of 3, then
	// one every 5s): this test refuses and conflicts more than that.
	s := newDeskSiteWith(t, "  all:\n    rate_limit:\n      enabled: false\n")
	s.visitAgain()
	var ids pageJSON[model.Identity]
	s.json(&ids, "identity", "list")
	if len(ids.Items) != 2 {
		t.Fatalf("identity list = %+v", ids.Items)
	}
	moving, staying := ids.Items[1], ids.Items[0]
	from, into := string(moving.Contact.ID), string(staying.Contact.ID)
	fromC, intoC := s.contact(from), s.contact(into)
	merge := []string{"contact", "merge", from, into, "--reason", "same_person"}

	// The preview: uma's identities and conversations, and the
	// revisions to merge at. It asks for no confirmation and writes
	// nothing.
	var plan planJSON
	s.json(&plan, append(merge, "--dry-run")...)
	gotIDs, gotConvs := planList(t, plan, "identities"), planList(t, plan, "conversations")
	if !slices.Equal(gotIDs, []string{string(moving.Ref.ID)}) || len(gotConvs) != 1 ||
		string(plan.Args["expected_revision"]) != `"`+string(fromC.Revision)+`"` ||
		string(plan.Args["expected_into_revision"]) != `"`+string(intoC.Revision)+`"` {
		t.Fatalf("merge preview = %+v", plan)
	}
	targets := map[string]bool{}
	for _, e := range plan.Effects {
		targets[e.Target] = true
	}
	for _, want := range []string{"contact:" + from, "contact:" + into, "identity:" + gotIDs[0], "conversation:" + gotConvs[0]} {
		if !targets[want] {
			t.Errorf("merge preview lacks effect on %s: %+v", want, plan.Effects)
		}
	}
	r := s.heo(0, append(merge, "--dry-run")...)
	for _, want := range []string{"identity:" + gotIDs[0], "conversation:" + gotConvs[0], string(fromC.Revision), string(intoC.Revision)} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("text merge preview lacks %q:\n%s", want, r.stdout)
		}
	}
	if got := s.contact(from); got.MergedInto != nil || got.Revision != fromC.Revision {
		t.Fatalf("merge --dry-run wrote: %+v", got)
	}
	// The conversation the preview names is owned by the identity it names.
	var conv model.Conversation
	s.json(&conv, "conversation", "get", gotConvs[0])
	if conv.Owner == nil || conv.Owner.ID != moving.Ref.ID {
		t.Fatalf("previewed conversation = %+v, owner is not %s", conv, moving.Ref.ID)
	}

	// Executing needs both revisions, a reason and the operator's
	// confirmation: never automatic.
	rv := []string{"--revision", string(fromC.Revision), "--into-revision", string(intoC.Revision)}
	wantStderr(t, s.heo(2, "contact", "merge", from, into, "--reason", "same_person", "--confirm=yes"), "USAGE", "revision")
	wantStderr(t, s.heo(2, "contact", "merge", from, into, "--reason", "same_person", "--revision", string(fromC.Revision), "--confirm=yes"), "USAGE", "into-revision")
	wantStderr(t, s.heo(2, "contact", "merge", from, into, "--revision", string(fromC.Revision), "--into-revision", string(intoC.Revision), "--confirm=yes"), "USAGE", "reason")
	wantStderr(t, s.heo(2, append(append(merge[:len(merge):len(merge)], rv...), "--confirm=yes", "--reason", "Same Person")...), "VALIDATION", "reason")
	wantStderr(t, s.heo(5, append(slices.Clone(merge), rv...)...), "UNAUTHORIZED", "confirm")
	if got := s.contact(from); got.MergedInto != nil {
		t.Fatalf("an unconfirmed merge ran: %+v", got)
	}
	// A stale revision, on either contact, moves nothing.
	wantStderr(t, s.heo(4, append(slices.Clone(merge), "--revision", string(fromC.Revision), "--into-revision", "stale", "--confirm=yes")...),
		"CONFLICT", "is at revision "+string(intoC.Revision))
	wantStderr(t, s.heo(4, append(slices.Clone(merge), "--revision", "stale", "--into-revision", string(intoC.Revision), "--confirm=yes")...),
		"CONFLICT", "is at revision "+string(fromC.Revision))
	// The identities held to the reviewed set.
	wantStderr(t, s.heo(4, append(append(slices.Clone(merge), rv...), "--identity", "id_other", "--confirm=yes")...), "CONFLICT", "identities")
	if got := s.contact(from); got.MergedInto != nil || got.Revision != fromC.Revision {
		t.Fatalf("a refused merge wrote: %+v", got)
	}

	// Merge.
	exec := append(append(slices.Clone(merge), rv...), "--confirm=yes", "--identity", string(moving.Ref.ID), "--idempotency-key", "mg-1")
	var done, again contactMergeJSON
	s.json(&done, exec...)
	if done.From.Ref.ID != moving.Contact.ID || done.From.MergedInto == nil || done.From.MergedInto.ID != staying.Contact.ID ||
		done.Into.MergedInto != nil || done.From.Revision == fromC.Revision || done.Into.Revision == intoC.Revision ||
		done.Operation.State != model.Committed || done.Operation.Replayed || done.OperationKey != "mg-1" {
		t.Fatalf("merge = %+v", done)
	}
	// What moved is what the preview listed.
	var moved []string
	for _, l := range done.Moved {
		if l.Class != model.LinkClassMerge || l.From != moving.Contact || l.To != staying.Contact ||
			l.Actor != model.ActorID(me()) || l.Reason != "same_person" || l.Assurance != model.LinkAssuranceAsserted {
			t.Errorf("merge link = %+v", l)
		}
		moved = append(moved, string(l.Identity.ID))
	}
	if !slices.Equal(moved, gotIDs) {
		t.Fatalf("moved %v, previewed %v", moved, gotIDs)
	}

	// The history shows the move; the contact merged reads as a redirect.
	v := s.identity(string(moving.Ref.ID))
	if v.Identity.Contact != staying.Contact || len(v.History) != 1 || v.History[0].Class != model.LinkClassMerge ||
		v.History[0].Operation.ID != done.Operation.ID {
		t.Fatalf("identity after merge = %+v", v)
	}
	r = s.heo(0, "identity", "get", string(moving.Ref.ID))
	for _, want := range []string{"link history:", "merge", me(), "same_person", into} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("identity get lacks %q:\n%s", want, r.stdout)
		}
	}
	r = s.heo(0, "contact", "get", from)
	if !strings.Contains(r.stdout, "merged into") || !strings.Contains(r.stdout, into) {
		t.Errorf("contact get of the contact merged does not read as redirected:\n%s", r.stdout)
	}
	var cv contactViewJSON
	if s.json(&cv, "contact", "get", from); cv.Contact.MergedInto == nil || cv.Contact.MergedInto.ID != staying.Contact.ID || len(cv.Identities) != 0 {
		t.Fatalf("contact get after merge = %+v", cv)
	}
	if s.json(&cv, "contact", "get", into); len(cv.Identities) != 2 {
		t.Fatalf("contact merged into after merge = %+v", cv)
	}
	r = s.heo(0, "contact", "list")
	if !strings.Contains(r.stdout, "MERGED INTO") || !strings.Contains(r.stdout, into) {
		t.Errorf("contact list does not show the redirect:\n%s", r.stdout)
	}

	// The same key and arguments replay; nothing moves twice.
	s.json(&again, exec...)
	if !again.Operation.Replayed || again.Operation.ID != done.Operation.ID || len(again.Moved) != len(done.Moved) {
		t.Fatalf("replayed merge = %+v", again)
	}
	if h := s.identity(string(moving.Ref.ID)).History; len(h) != 1 {
		t.Fatalf("history after replay = %+v", h)
	}
	// Another key, or a merge into or out of a merged contact, conflicts.
	other := append(append(slices.Clone(merge), rv...), "--confirm=yes", "--idempotency-key", "mg-2")
	wantStderr(t, s.heo(4, other...), "CONFLICT", "already merged into "+into)
	wantStderr(t, s.heo(4, "contact", "merge", into, from, "--reason", "same_person", "--dry-run"), "CONFLICT", "merge into that one")
	wantStderr(t, s.heo(4, "contact", "merge", from, into, "--reason", "same_person", "--revision", string(done.From.Revision),
		"--into-revision", string(done.Into.Revision), "--confirm=yes", "--idempotency-key", "mg-3"), "CONFLICT", "already merged")
	// A link to the contact merged is refused: the redirect says where
	// its identities went.
	wantStderr(t, s.heo(4, "identity", "link", string(staying.Ref.ID), "--contact", from, "--revision", string(s.identity(string(staying.Ref.ID)).Identity.Revision)),
		"CONFLICT", "merged")
	s.stop(s.d)

	// A viewer reads the redirect and merges nothing.
	s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
	s.d = s.start()
	if s.json(&cv, "contact", "get", from); cv.Contact.MergedInto == nil {
		t.Fatalf("viewer contact get = %+v", cv)
	}
	wantStderr(t, s.heo(5, "contact", "merge", into, from, "--reason", "same_person", "--dry-run"), "UNAUTHORIZED", "heo.authz.not_granted")
	wantStderr(t, s.heo(5, "contact", "merge", into, from, "--reason", "same_person", "--revision", "x", "--into-revision", "y", "--confirm=yes"),
		"UNAUTHORIZED", "heo.authz.not_granted")
	s.stop(s.d)
}
