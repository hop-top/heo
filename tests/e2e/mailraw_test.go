package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/model"
)

// TestMessageRawLifecycle drives the e2e build on Postgres through a raw
// export on a fake provider that declares raw_read: the provider's bytes
// reach stdout alone, a file (owner-only, never overwritten without
// --force) or a directory; --format json with -o reports the file and
// never the bytes; a read leaves no operation behind and repeats. An
// adapter that declares no raw_read, a message of an unknown ID and a
// role that does not read are refused.
func TestMessageRawLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		s := newEffectSite(t, noRateLimit)
		s.mail(s.mb, "mbin", "mb-1", "ev-mb", "mb-msg-1")
		var msgs pageJSON[model.Message]
		s.json(&msgs, "message", "list", "--inbox", "mbin")
		if len(msgs.Items) != 1 || len(msgs.Items[0].External) == 0 {
			t.Fatalf("messages = %+v", msgs.Items)
		}
		id := string(msgs.Items[0].Ref.ID)
		want := string(adapterstest.RawBytes(msgs.Items[0].External[0].ID))
		before := s.ledger()

		// Stdout is the default and carries the bytes alone.
		r := s.heo(0, "message", "raw", id)
		if r.stdout != want || r.stderr != "" {
			t.Fatalf("stdout = %q, stderr = %q, want the provider's bytes alone", r.stdout, r.stderr)
		}
		if r := s.heo(0, "message", "raw", id, "-o", "-", "--conversation", string(msgs.Items[0].Ref.Conversation.ID)); r.stdout != want {
			t.Fatalf("-o - = %q", r.stdout)
		}

		// A file: owner-only, never overwritten without --force.
		cwd := s.s.path("cwd")
		out := filepath.Join(cwd, "m.eml")
		r = s.heo(0, "message", "raw", id, "-o", out)
		if got := readFile(t, out); got != want {
			t.Fatalf("file = %q", got)
		}
		if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
		}
		for _, w := range []string{id, "message/rfc822", "path"} {
			if !strings.Contains(r.stdout, w) {
				t.Errorf("summary lacks %q:\n%s", w, r.stdout)
			}
		}
		wantStderr(t, s.heo(4, "message", "raw", id, "-o", out), "CONFLICT", "--force")
		if err := os.WriteFile(out, []byte("local edits"), 0o600); err != nil {
			t.Fatal(err)
		}
		wantStderr(t, s.heo(4, "message", "raw", id, "-o", out), "CONFLICT")
		if readFile(t, out) != "local edits" {
			t.Fatal("a refused export overwrote the file")
		}
		s.heo(0, "message", "raw", id, "-o", out, "--force")
		if readFile(t, out) != want {
			t.Fatal("--force did not restore the bytes")
		}

		// A directory gets <message>.eml; --format json with -o reports
		// the file on stdout and holds no bytes.
		dir := filepath.Join(cwd, "mail")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		var rep struct {
			Message   string
			MediaType string `json:"media_type"`
			Size      int64
			Path      string
			Content   string
		}
		s.json(&rep, "message", "raw", id, "-o", dir)
		if rep.Path != filepath.Join(dir, id+".eml") || rep.Size != int64(len(want)) || rep.MediaType != "message/rfc822" ||
			rep.Message != id || rep.Content != "" || readFile(t, rep.Path) != want {
			t.Fatalf("json summary = %+v", rep)
		}

		// A read is no write.
		if n := s.ledger(); n != before {
			t.Errorf("a raw read left operations: %d, was %d", n, before)
		}

		// Refusals.
		pl := s.mail(s.plain, "plmail", "pl-1", "ev-pl", "plain-msg-1")
		var pm pageJSON[model.Message]
		s.json(&pm, "message", "list", "--conversation", string(pl.Ref.ID))
		wantStderr(t, s.heo(2, "message", "raw", string(pm.Items[0].Ref.ID)), "UNSUPPORTED", "raw_read")
		wantStderr(t, s.heo(3, "message", "raw", "00000ad0-0000-7000-8000-0000000009ff"), "NOT_FOUND")
		wantStderr(t, s.heo(2, "message", "raw"), "")

		s.stop(s.d)
	})
}
