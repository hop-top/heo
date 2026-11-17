package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"hop.top/heo/internal/composition"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

type identityViewJSON struct {
	Identity model.Identity
	History  []model.IdentityLink
}

type contactViewJSON struct {
	Contact    model.Contact
	Identities []model.Identity
}

type identityChangeJSON struct {
	Identity     model.Identity
	Link         *model.IdentityLink
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

// visitAgain has another widget visitor open a session and write once.
func (s *deskSite) visitAgain() {
	s.t.Helper()
	code, body := postJSON(s.t, s.route+"/session?"+composition.WidgetSiteParam+"=site-1", "{}")
	var sess widget.SessionResponse
	if code != http.StatusOK || json.Unmarshal([]byte(body), &sess) != nil {
		s.t.Fatalf("session = %d %s", code, body)
	}
	if code, body := postJSON(s.t, s.route+"/messages", `{"id":"m-2","text":"hi again"}`, "Authorization", "Bearer "+sess.Token); code != http.StatusAccepted {
		s.t.Fatalf("visitor message = %d %s", code, body)
	}
}

// hasRow reports whether out has a two-column row key value.
// hasRow reports whether out holds a "key  value" row; key may hold spaces.
func hasRow(out, key, value string) bool {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), key)
		if ok && strings.HasPrefix(rest, " ") && strings.TrimSpace(rest) == value {
			return true
		}
	}
	return false
}

func (s *deskSite) identity(id string) identityViewJSON {
	s.t.Helper()
	var v identityViewJSON
	s.json(&v, "identity", "get", id)
	return v
}

// TestContactAndIdentityCommands drives the release binary on Postgres
// through two real widget visitors: each inbound sender is an identity
// on a provisional contact of its own, listed and shown; a desk actor
// links one visitor's identity to the other's contact on its
// attestation, at the identity's revision — a preview writes nothing, a
// stale revision is a conflict, the same key replays — and the link
// history shows the move with its actor; an unlink needs a reason and is
// recorded too; a viewer reads contacts and identities but is refused
// linking and unlinking.
func TestContactAndIdentityCommands(t *testing.T) {
	s := newDeskSite(t)
	s.visitAgain()

	// Inbound: two identities, each on a provisional contact of its own.
	var ids pageJSON[model.Identity]
	s.json(&ids, "identity", "list")
	if len(ids.Items) != 2 || ids.Items[0].Contact == ids.Items[1].Contact {
		t.Fatalf("identity list = %+v", ids.Items)
	}
	first, second := ids.Items[0], ids.Items[1]
	for _, i := range ids.Items {
		if i.Kind != model.ContactKindPerson || i.LinkAssurance != model.LinkAssuranceNone || i.Ref.Inbox != s.conv.Inbox {
			t.Fatalf("identity = %+v", i)
		}
	}
	var contacts pageJSON[model.Contact]
	s.json(&contacts, "contact", "list")
	if len(contacts.Items) != 2 {
		t.Fatalf("contact list = %+v", contacts.Items)
	}
	s.json(&ids, "identity", "list", "--contact", string(first.Contact.ID))
	if len(ids.Items) != 1 || ids.Items[0].Ref != first.Ref {
		t.Fatalf("identity list --contact = %+v", ids.Items)
	}
	r := s.heo(0, "identity", "list", "--inbox", "webin")
	for _, want := range []string{string(first.Ref.ID), string(second.Ref.ID), "none"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("identity list lacks %q:\n%s", want, r.stdout)
		}
	}
	r = s.heo(0, "contact", "get", string(first.Contact.ID))
	if !strings.Contains(r.stdout, string(first.Ref.ID)) || !strings.Contains(r.stdout, "person") {
		t.Errorf("contact get:\n%s", r.stdout)
	}
	wantStderr(t, s.heo(3, "contact", "get", "ct_404"), "NOT_FOUND")
	wantStderr(t, s.heo(3, "identity", "get", "id_404"), "NOT_FOUND")
	r = s.heo(0, "identity", "get", string(second.Ref.ID))
	if !strings.Contains(r.stdout, "link history: none") {
		t.Errorf("identity get before any link:\n%s", r.stdout)
	}

	// Link the second visitor's identity to the first's contact.
	id, target := string(second.Ref.ID), string(first.Contact.ID)
	link := []string{"identity", "link", id, "--contact", target}
	wantStderr(t, s.heo(2, "identity", "link", id, "--revision", string(second.Revision)), "USAGE", "contact")
	wantStderr(t, s.heo(2, link...), "USAGE", "revision")
	wantStderr(t, s.heo(4, append(link, "--revision", "stale")...), "CONFLICT", "is at revision "+string(second.Revision))
	var plan planJSON
	s.json(&plan, append(link, "--revision", string(second.Revision), "--dry-run")...)
	if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || plan.Effects[0].Target != "identity:"+id ||
		string(plan.Args["class"]) != `"link"` || string(plan.Args["assurance"]) != `"asserted"` ||
		string(plan.Args["evidence"]) != `"attestation"` || !strings.Contains(plan.Effects[0].Detail, "association only") {
		t.Fatalf("link preview = %+v", plan)
	}
	r = s.heo(0, append(link, "--revision", string(second.Revision), "--dry-run")...)
	for _, want := range [][2]string{{"contact", target}, {"class", "link"}, {"evidence", "attestation"}, {"assurance", "asserted"}} {
		if !hasRow(r.stdout, want[0], want[1]) {
			t.Errorf("text link preview lacks row %v:\n%s", want, r.stdout)
		}
	}
	if v := s.identity(id); v.Identity.Revision != second.Revision || v.Identity.Contact != second.Contact || len(v.History) != 0 {
		t.Fatalf("link --dry-run wrote: %+v", v)
	}

	var linked, again identityChangeJSON
	linkAt := append(link, "--revision", string(second.Revision), "--reference", "TICKET-1", "--idempotency-key", "link-1")
	s.json(&linked, linkAt...)
	if linked.Identity.Contact != first.Contact || linked.Identity.LinkAssurance != model.LinkAssuranceAsserted ||
		linked.Operation.State != model.Committed || linked.Operation.Replayed || linked.OperationKey != "link-1" ||
		linked.Link == nil || linked.Link.Class != model.LinkClassLink || linked.Link.Actor != model.ActorID(me()) ||
		linked.Link.From != second.Contact || linked.Link.To != first.Contact || linked.Link.Evidence.Reference != "TICKET-1" {
		t.Fatalf("link = %+v", linked)
	}
	s.json(&again, linkAt...)
	if !again.Operation.Replayed || again.Operation.ID != linked.Operation.ID {
		t.Fatalf("replayed link = %+v", again)
	}

	// The link history shows the move, with its actor.
	v := s.identity(id)
	if len(v.History) != 1 || v.History[0].Actor != model.ActorID(me()) || v.History[0].To != first.Contact ||
		v.History[0].Assurance != model.LinkAssuranceAsserted || v.History[0].Operation.ID != linked.Operation.ID {
		t.Fatalf("history after link = %+v", v.History)
	}
	r = s.heo(0, "identity", "get", id)
	for _, want := range []string{"link history:", "link", me(), "attestation TICKET-1", target} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("identity get lacks %q:\n%s", want, r.stdout)
		}
	}
	var cv contactViewJSON
	s.json(&cv, "contact", "get", target)
	if len(cv.Identities) != 2 {
		t.Fatalf("contact get after link = %+v", cv)
	}

	// Unlink: the reason is required and must be a reason code.
	rev := string(v.Identity.Revision)
	unlink := []string{"identity", "unlink", id}
	wantStderr(t, s.heo(2, append(unlink, "--revision", rev)...), "USAGE", "reason")
	wantStderr(t, s.heo(2, append(unlink, "--revision", rev, "--reason", "Wrong Person")...), "VALIDATION", "reason")
	wantStderr(t, s.heo(4, append(unlink, "--revision", string(second.Revision), "--reason", "wrong_person")...), "CONFLICT", "is at revision "+rev)
	s.json(&plan, append(unlink, "--revision", rev, "--reason", "wrong_person", "--dry-run")...)
	if len(plan.Effects) != 1 || string(plan.Args["reason"]) != `"wrong_person"` || string(plan.Args["class"]) != `"unlink"` {
		t.Fatalf("unlink preview = %+v", plan)
	}
	if got := s.identity(id); got.Identity.Revision != v.Identity.Revision || len(got.History) != 1 {
		t.Fatalf("unlink --dry-run wrote: %+v", got)
	}
	var unlinked identityChangeJSON
	s.json(&unlinked, append(unlink, "--revision", rev, "--reason", "wrong_person")...)
	if c := unlinked.Identity.Contact; c == first.Contact || c == second.Contact || unlinked.Link == nil ||
		unlinked.Link.Class != model.LinkClassUnlink || unlinked.Link.Reason != "wrong_person" ||
		unlinked.Link.Actor != model.ActorID(me()) || unlinked.Identity.LinkAssurance != model.LinkAssuranceNone {
		t.Fatalf("unlink = %+v", unlinked)
	}
	if h := s.identity(id).History; len(h) != 2 || h[1].Class != model.LinkClassUnlink || h[1].Reason != "wrong_person" {
		t.Fatalf("history after unlink = %+v", h)
	}
	s.stop(s.d)

	// A viewer reads contacts and identities, and links nothing.
	s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
	s.d = s.start()
	s.json(&ids, "identity", "list")
	if len(ids.Items) != 2 {
		t.Fatalf("viewer identity list = %+v", ids.Items)
	}
	s.json(&contacts, "contact", "list")
	if len(contacts.Items) != 3 {
		t.Fatalf("viewer contact list = %+v", contacts.Items)
	}
	cur := s.identity(id)
	if len(cur.History) != 2 {
		t.Fatalf("viewer history = %+v", cur.History)
	}
	crev := string(cur.Identity.Revision)
	wantStderr(t, s.heo(5, "identity", "link", id, "--contact", target, "--revision", crev), "UNAUTHORIZED", "heo.authz.not_granted")
	wantStderr(t, s.heo(5, "identity", "unlink", id, "--reason", "wrong_person", "--revision", crev), "UNAUTHORIZED", "heo.authz.not_granted")
	if got := s.identity(id); got.Identity.Revision != cur.Identity.Revision || len(got.History) != 2 {
		t.Fatalf("viewer wrote: %+v", got)
	}
	s.stop(s.d)
}
