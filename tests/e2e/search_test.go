package e2e

import (
	"slices"
	"strings"
	"testing"

	"hop.top/uma/model"
)

// searchPage is one page of a message search with its cursor.
type searchPage struct {
	Items    []model.Message
	Next     model.Cursor
	Complete bool
}

// search runs message list with args and returns the texts found, sorted.
func (s *deskSite) search(args ...string) []string {
	s.t.Helper()
	var p searchPage
	s.json(&p, append([]string{"message", "list"}, args...)...)
	return texts(p.Items)
}

func texts(ms []model.Message) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		var parts []string
		for _, p := range m.Content.Parts {
			parts = append(parts, p.Text)
		}
		out = append(out, strings.Join(parts, "\n"))
	}
	slices.Sort(out)
	return out
}

// TestMessageSearch drives the release binary on Postgres through a real
// widget visitor: message list searches by text, time and conversation
// label, uma applying every filter; combined filters narrow; paging
// follows the cursor to completion; a bad time or blank text is a usage
// error and uma's refusals pass through; and the internal note a desk
// actor finds is never found by a viewer or an external actor, whatever
// words they search, while an external actor may not filter by label.
func TestMessageSearch(t *testing.T) {
	s := newDeskSite(t)
	id := string(s.conv.ID)
	const (
		reply = "your invoice refund is on its way"
		note  = "refund needs finance sign-off"
	)
	s.json(&replyJSON{}, "message", "reply", id, "--text", reply)
	s.json(&noteJSON{}, "note", "add", id, "--text", note, "--placement", "local")

	// Text: every word, any order, whole words, case folded.
	if got := s.search("--text", "refund"); !slices.Equal(got, []string{note, reply}) {
		t.Fatalf("search refund = %q", got)
	}
	if got := s.search("--text", "FINANCE refund"); !slices.Equal(got, []string{note}) {
		t.Fatalf("search FINANCE refund = %q", got)
	}
	if got := s.search("--text", "hello"); !slices.Equal(got, []string{"hello"}) {
		t.Fatalf("search hello = %q", got)
	}
	if got := s.search("--text", "refun"); len(got) != 0 {
		t.Fatalf("search refun = %q, want whole words only", got)
	}
	if got := s.search("--text", "refund", "--conversation", id, "--inbox", "webin"); len(got) != 2 {
		t.Fatalf("search in conversation = %q", got)
	}
	r := s.heo(0, "message", "list", "--text", "finance")
	if !strings.Contains(r.stdout, "internal") || strings.Contains(r.stdout, "invoice") {
		t.Fatalf("text search:\n%s", r.stdout)
	}

	// Time: [since, until), a date or an instant.
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"--since", "2000-01-01"}, 3},
		{[]string{"--since", "2000-01-01T00:00:00Z", "--until", "2999-01-01"}, 3},
		{[]string{"--since", "2999-01-01"}, 0},
		{[]string{"--until", "2000-01-01"}, 0},
		{[]string{"--text", "refund", "--since", "2000-01-01", "--until", "2999-01-01"}, 2},
		{[]string{"--text", "refund", "--since", "2999-01-01"}, 0},
	} {
		if got := s.search(tc.args...); len(got) != tc.want {
			t.Errorf("search %v = %q, want %d", tc.args, got, tc.want)
		}
	}
	wantStderr(t, s.heo(2, "message", "list", "--since", "yesterday"), "USAGE", "--since")
	wantStderr(t, s.heo(2, "message", "list", "--until", "2026-13-40"), "USAGE", "--until")
	wantStderr(t, s.heo(2, "message", "list", "--text", " "), "USAGE", "--text")
	wantStderr(t, s.heo(2, "message", "list", "--since", "2999-01-01", "--until", "2000-01-01"), "VALIDATION")
	wantStderr(t, s.heo(2, "message", "list", "--text", "!!!"), "VALIDATION")

	// Labels, passed to uma beside the text; an unknown label is not
	// found, with the command that lists them.
	s.heo(0, "label", "create", "billing")
	s.heo(0, "label", "create", "urgent")
	s.heo(0, "conversation", "label", id, "--add", "billing")
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"--text", "refund", "--label", "billing"}, 2},
		{[]string{"--text", "refund", "--label", "billing,urgent"}, 2},
		{[]string{"--text", "refund", "--label-all", "billing,urgent"}, 0},
		{[]string{"--text", "refund", "--label-none", "urgent"}, 2},
		{[]string{"--text", "refund", "--label-none", "billing"}, 0},
		{[]string{"--text", "nothing", "--label", "billing"}, 0},
		{[]string{"--label", "billing"}, 3},
	} {
		if got := s.search(tc.args...); len(got) != tc.want {
			t.Errorf("search %v = %q, want %d", tc.args, got, tc.want)
		}
	}
	wantStderr(t, s.heo(3, "message", "list", "--label", "nope"), "NOT_FOUND", "heo label list")
	s.heo(0, "conversation", "label", id, "--remove", "billing")
	if got := s.search("--text", "refund", "--label", "billing"); len(got) != 0 {
		t.Fatalf("search after the label was removed = %q", got)
	}

	// Paging: one match a page, the cursor followed to completion.
	var found []string
	var cursor string
	for i := 0; ; i++ {
		args := []string{"message", "list", "--text", "refund", "--limit", "1"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		var p searchPage
		s.json(&p, args...)
		found = append(found, texts(p.Items)...)
		if p.Complete {
			break
		}
		if p.Next == "" || i > 10 {
			t.Fatalf("page %d = %+v: incomplete without a cursor", i, p)
		}
		cursor = string(p.Next)
	}
	slices.Sort(found)
	if !slices.Equal(found, []string{note, reply}) {
		t.Fatalf("paged search = %q", found)
	}
	s.stop(s.d)

	// A viewer and an external actor read what is public: the note is
	// found by neither, by any of its words.
	for _, role := range []string{"viewer", "external"} {
		s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: ["+role+"]\n")
		d := s.start()
		for _, words := range []string{"finance", "sign-off", "refund finance", "needs"} {
			if got := s.search("--text", words); len(got) != 0 {
				t.Fatalf("%s found the note by %q: %q", role, words, got)
			}
		}
		if got := s.search("--text", "refund"); !slices.Equal(got, []string{reply}) {
			t.Fatalf("%s search refund = %q, want the public reply only", role, got)
		}
		if got := s.search("--text", "refund", "--since", "2000-01-01"); !slices.Equal(got, []string{reply}) {
			t.Fatalf("%s search refund since = %q", role, got)
		}
		if role == "viewer" {
			if got := s.search("--text", "refund", "--label-none", "urgent"); !slices.Equal(got, []string{reply}) {
				t.Fatalf("viewer label search = %q", got)
			}
		} else {
			wantDenied(t, s.s.run(nil, "message", "list", "--label", "billing"), "heo.authz.not_granted")
		}
		s.stop(d)
	}
}
