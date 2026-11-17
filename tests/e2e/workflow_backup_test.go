package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
	umapg "hop.top/uma/store/postgres"
)

// workflowSchema is the uma schema that added the conversation workflow
// (assignee and snooze columns); the one after it added links and read
// markers.
const workflowSchema = 20

// workflowState is what a conversation shows of its workflow state, and
// the caller's read marker, comparable across a restore.
type workflowState struct {
	Revision model.Revision
	Assignee string
	Snooze   string
	Links    []string
	Marked   bool
	Through  time.Time
}

func (s *deskSite) workflowOf(id model.ID) workflowState {
	s.t.Helper()
	v := s.view(id)
	w := workflowState{Revision: v.Revision}
	if v.Assignee != nil {
		w.Assignee = string(*v.Assignee)
	}
	if v.Snooze != nil {
		w.Snooze = v.Snooze.Until.UTC().Format(time.RFC3339Nano) + " " + string(v.Snooze.By)
	}
	for _, l := range v.Links {
		if l.Conversation != nil {
			w.Links = append(w.Links, string(l.Conversation.ID))
		}
	}
	if v.Read != nil {
		w.Marked, w.Through = v.Read.Marked, v.Read.ReadThrough
	}
	return w
}

// TestStorageBackupRestoreCarriesWorkflowState drives the release binary
// on Postgres: assignments, snoozes, links and read markers live in uma's
// own tables, so a backup holds them and a restore brings them back
// exactly, after the profile moved on; an external participant sees none
// of it afterwards.
func TestStorageBackupRestoreCarriesWorkflowState(t *testing.T) {
	withE2EBinary(t, func() {
		s := newDeskSiteWith(t, noBridge)
		s.restartAs(workflowProfile)
		a, va := s.conv, &visitor{token: s.token, conv: s.conv}
		b := newVisitor(t, s).conv
		aid, bid := string(a.ID), string(b.ID)
		va.say(t, s, "more")
		first := s.messages("--conversation", aid)[0].Ref.ID
		var out workflowOutJSON
		var mark readMarkOutJSON
		s.json(&out, "conversation", "assign", aid, "--to", deskPeer)
		s.json(&out, "conversation", "snooze", bid, "--for", "2h")
		s.json(&out, "conversation", "link", aid, "--conversation", bid)
		s.json(&mark, "conversation", "mark-read", aid, "--through", string(first))
		s.json(&mark, "conversation", "mark-read", bid)
		wantA, wantB := s.workflowOf(a.ID), s.workflowOf(b.ID)
		if wantA.Assignee != deskPeer || !wantA.Marked || len(wantA.Links) != 1 || wantB.Snooze == "" || len(wantB.Links) != 1 || !wantB.Marked {
			t.Fatalf("the state to restore: a %+v, b %+v", wantA, wantB)
		}
		s.stop(s.d)

		pgDump, pgRestore := pgDumpInContainer(t, s.s), pgRestoreInContainer(t, s.s)
		backups := s.s.path("backups")
		tools := []string{"-c", "storage.postgres.pg_dump=" + pgDump, "-c", "storage.postgres.pg_restore=" + pgRestore, "-c", "storage.backup.dir=" + backups}
		storageCmd := func(exit int, args ...string) result {
			t.Helper()
			full := append(append([]string{}, args...), tools...)
			r := s.s.run(nil, full...)
			s.s.want(r, exit, full...)
			return r
		}
		var bk struct{ Path string }
		r := storageCmd(0, "storage", "backup", "--format", "json")
		if err := json.Unmarshal([]byte(lastJSON(r.stdout)), &bk); err != nil || bk.Path == "" {
			t.Fatalf("backup = %q: %v", r.stdout, err)
		}
		// The dump names the tables the state lives in.
		dump := archiveEntry(t, bk.Path, "store.dump")
		for _, table := range []string{"uma_conversations", "uma_read_markers"} {
			if !bytes.Contains(dump, []byte(table)) {
				t.Errorf("the backup does not hold %s", table)
			}
		}

		// The profile moves on: everything is undone, and a new marker.
		s.d = s.start()
		var undo workflowOutJSON
		s.json(&undo, "conversation", "unassign", aid)
		s.json(&undo, "conversation", "unsnooze", bid)
		s.json(&undo, "conversation", "unlink", aid, "--conversation", bid)
		s.json(&mark, "conversation", "mark-unread", aid)
		s.json(&mark, "conversation", "mark-unread", bid)
		if now := s.workflowOf(a.ID); now.Assignee != "" || len(now.Links) != 0 || now.Through.Equal(wantA.Through) {
			t.Fatalf("the profile did not move on: %+v", now)
		}
		s.stop(s.d)

		storageCmd(0, "storage", "restore", bk.Path, "--confirm=yes")
		s.d = s.start()
		gotA, gotB := s.workflowOf(a.ID), s.workflowOf(b.ID)
		if fmt.Sprint(gotA) != fmt.Sprint(wantA) || fmt.Sprint(gotB) != fmt.Sprint(wantB) {
			t.Fatalf("after the restore:\n a %+v\n want %+v\n b %+v\n want %+v", gotA, wantA, gotB, wantB)
		}
		if got := s.listed("--assignee", deskPeer); !slices.Equal(got, refIDs(a)) {
			t.Errorf("--assignee after the restore = %v", got)
		}
		if got := s.listed("--snoozed", "only"); !slices.Equal(got, refIDs(b)) {
			t.Errorf("--snoozed only after the restore = %v", got)
		}
		if got := s.listed("--linked", "--snoozed", "include"); !slices.Equal(got, refIDs(a, b)) {
			t.Errorf("--linked after the restore = %v", got)
		}
		if got := s.listed("--unread", "--snoozed", "include"); !slices.Equal(got, refIDs(a)) {
			t.Errorf("--unread after the restore = %v (a has a message past its marker)", got)
		}

		// Restored, an external participant sees none of it.
		s.restartAs("authorization:\n  owner: [external]\n")
		if x := s.view(a.ID); x.Assignee != nil || x.Snooze != nil || len(x.Links) != 0 || x.Read != nil || x.Workflow {
			t.Fatalf("external sees restored workflow state: %+v", x)
		}
		if x := s.view(b.ID); x.Snooze != nil || len(x.Links) != 0 {
			t.Fatalf("external sees a restored snooze or link: %+v", x)
		}
		if got := s.listed(); !slices.Equal(got, refIDs(a, b)) {
			t.Errorf("external list after the restore = %v", got)
		}
		if got := s.eventsOf(workflowKinds...); len(got) != 0 {
			t.Errorf("external lists %d workflow events", len(got))
		}
	})
}

// TestStoreBeforeWorkflowMigrates: a store at the schema before the
// conversation workflow is refused at start with the way forward;
// `storage migrate` backs it up and brings it to this binary's schema,
// which backfills the assignee and snooze columns from the conversations
// already stored, so the filters find them; the links the records hold
// survive, and a read marker is the one thing the older schema had no
// table for.
func TestStoreBeforeWorkflowMigrates(t *testing.T) {
	if umapg.SchemaVersion < workflowSchema+1 {
		t.Fatalf("uma schema %d predates the workflow (%d)", umapg.SchemaVersion, workflowSchema)
	}
	withE2EBinary(t, func() {
		s := newDeskSiteWith(t, noBridge)
		s.restartAs(workflowProfile)
		a := s.conv
		b := newVisitor(t, s).conv
		aid, bid := string(a.ID), string(b.ID)
		var out workflowOutJSON
		s.json(&out, "conversation", "assign", aid, "--to", deskPeer)
		s.json(&out, "conversation", "snooze", bid, "--for", "2h")
		s.json(&out, "conversation", "link", aid, "--conversation", bid)
		s.stop(s.d)

		// What an older release left: the workflow columns and tables are
		// gone, the records' bodies are not.
		s.db.downgradeTo(workflowSchema - 1)
		r := s.s.run(nil, "service", "start")
		s.s.want(r, 1, "service", "start")
		for _, want := range []string{
			fmt.Sprintf("store schema version %d is older than %d", workflowSchema-1, umapg.SchemaVersion),
			"run `heo storage migrate`",
		} {
			if !strings.Contains(r.stderr, want) {
				t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
			}
		}
		args := []string{
			"storage", "migrate", "--format", "json", "-c", "storage.postgres.pg_dump=" + pgDumpInContainer(t, s.s),
			"-c", "storage.backup.dir=" + s.s.path("backups"),
		}
		r = s.s.run(nil, args...)
		s.s.want(r, 0, args...)
		var m struct {
			From, To int64
			Backup   *struct{ Schema int64 }
		}
		if err := json.Unmarshal([]byte(lastJSON(r.stdout)), &m); err != nil {
			t.Fatalf("%v: %s", err, r.stdout)
		}
		if m.From != workflowSchema-1 || m.To != umapg.SchemaVersion || m.Backup == nil || m.Backup.Schema != workflowSchema-1 {
			t.Fatalf("migrate = %+v, want %d → %d after a backup", m, workflowSchema-1, umapg.SchemaVersion)
		}

		s.d = s.start()
		if got := s.listed("--assignee", deskPeer); !slices.Equal(got, refIDs(a)) {
			t.Errorf("--assignee after the migration = %v", got)
		}
		if got := s.listed("--snoozed", "only"); !slices.Equal(got, refIDs(b)) {
			t.Errorf("--snoozed only after the migration = %v", got)
		}
		if got := s.listed("--linked", "--snoozed", "include"); !slices.Equal(got, refIDs(a, b)) {
			t.Errorf("--linked after the migration = %v", got)
		}
		if got := s.listed(); !slices.Equal(got, refIDs(a)) {
			t.Errorf("default list after the migration = %v (b is snoozed)", got)
		}
		// The new tables work: a marker, a change at the stored revision.
		var mark readMarkOutJSON
		s.json(&mark, "conversation", "mark-read", aid)
		if !mark.Changed {
			t.Errorf("mark-read after the migration = %+v", mark)
		}
		s.json(&out, "conversation", "unassign", aid)
		if !out.Changed {
			t.Errorf("unassign after the migration = %+v", out)
		}
	})
}
