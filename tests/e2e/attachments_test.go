package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/blobs"
	"hop.top/heo/internal/desk"
	kitlocal "hop.top/kit/go/storage/blob/local"
	"hop.top/uma/model"
)

// inject has the daemon's fake channel accept an inbound message, as
// its provider would, and waits for the channel's webhook to be there.
func (c *cliSite) inject(ch model.Channel, in adapterstest.Inject) {
	c.t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		c.t.Fatal(err)
	}
	url := "http://" + c.hook + "/webhooks/" + string(ch.Ref.ID) + adapterstest.InjectRoute
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:noctx,gosec // loopback test URL
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusServiceUnavailable {
				c.t.Fatalf("inject %s = %d %s", in.Event, resp.StatusCode, b)
			}
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("inject %s: the channel's webhook never answered (%v)", in.Event, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// received injects a message with attachments for the files inbox and
// returns it as the desk lists it.
func (c *cliSite) received(ch model.Channel, event string, atts ...adapterstest.InjectAttachment) model.Message {
	c.t.Helper()
	text := "files " + event
	c.inject(ch, adapterstest.Inject{Event: event, Message: event, From: "ada", Destination: "files-1", Text: text, Attachments: atts})
	var msgs pageJSON[model.Message]
	c.json(&msgs, "message", "list", "--inbox", "files")
	for _, m := range msgs.Items {
		if desk.Text(m.Content) == text {
			return m
		}
	}
	c.t.Fatalf("message %s not listed: %+v", event, msgs.Items)
	return model.Message{}
}

func (c *cliSite) attachments(args ...string) []desk.Attachment {
	c.t.Helper()
	var p pageJSON[desk.Attachment]
	c.json(&p, append([]string{"attachment", "list"}, args...)...)
	return p.Items
}

func attachmentRef(m model.Message, n int) string {
	var refs []model.AttachmentRef
	for _, p := range m.Content.Parts {
		if p.Attachment != nil {
			refs = append(refs, *p.Attachment)
		}
	}
	return desk.RefName(refs[n])
}

// blobFiles lists the files under dir.
func blobFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// blobCount is the number of blobs the backend holds under dir: its
// own bookkeeping files are not blobs.
func blobCount(t *testing.T, dir string) int {
	t.Helper()
	st, err := kitlocal.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := st.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	return len(objs)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // G304: a file the test made
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// marker is bytes no log, summary or error may ever carry.
const marker = "ATTACHMENT-BYTES-MARKER-9f3a"

// TestAttachmentCommands drives the e2e build on Postgres with a
// provider that hands the channel messages with media: the desk lists,
// inspects and downloads the attachments; a file is never overwritten
// without --force; -o - streams the bytes to stdout alone; the bytes
// reach no log; an external participant is denied once the provider
// narrows the message that holds the attachment, and reads one that
// stayed public; a redacted attachment is gone and its bytes removed; a
// role that does not read is refused.
func TestAttachmentCommands(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		blobDir := c.s.path("data", "heo", "default", "blobs")
		store, err := blobs.Open(blobDir)
		if err != nil {
			t.Fatal(err)
		}
		c.db.blobs = store
		d := c.start()
		ch := c.fakeChannel("fake", adapterstest.Config{Account: "acct-att"})
		c.heo(0, "inbox", "create", "--channel", "fake", "--key", "files", "--destination", "files-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)

		binary := []byte{0x00, 0xff, 0xfe, 0x0a, 0x0d, 0x1b, '[', '3', '1', 'm', 0x80, 0x00}
		invoice := marker + " invoice"
		m1 := c.received(ch, "m1",
			adapterstest.InjectAttachment{Key: "k-invoice", Name: "invoice.txt", MediaType: "text/plain", Data: []byte(invoice)},
			adapterstest.InjectAttachment{Key: "k-blob", Name: "../../evil/photo.bin", MediaType: "application/octet-stream", Data: binary})
		m2 := c.received(ch, "m2",
			adapterstest.InjectAttachment{Key: "k-invoice", Name: "invoice.txt", MediaType: "text/plain", Data: []byte(invoice)})
		public := c.received(ch, "public",
			adapterstest.InjectAttachment{Key: "k-public", Name: "public.txt", MediaType: "text/plain", Data: []byte("public bytes")})
		doomed := c.received(ch, "doomed",
			adapterstest.InjectAttachment{Key: "k-doomed", Name: "pii.txt", MediaType: "text/plain", Data: []byte("personal data")})
		invoiceRef, photoRef := attachmentRef(m1, 0), attachmentRef(m1, 1)
		publicRef, doomedRef := attachmentRef(public, 0), attachmentRef(doomed, 0)
		conv := string(m1.Ref.Conversation.ID)

		// Owner-only: the root is 0700 whatever the files in it are.
		if fi, err := os.Stat(blobDir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("blob dir = %v, %v; want 0700", fi, err)
		}

		// List: by message and by conversation, metadata and holders, no bytes.
		got := c.attachments("--message", string(m1.Ref.ID))
		if len(got) != 2 || got[0].Ref != invoiceRef || got[0].Name != "invoice.txt" || got[0].MediaType != "text/plain" ||
			got[0].Size != int64(len(invoice)) || !strings.HasPrefix(got[0].Digest, "sha256:") ||
			got[0].Audience.Visibility != model.Public || got[1].Ref != photoRef || got[1].Size != int64(len(binary)) {
			t.Fatalf("list --message = %+v", got)
		}
		byConv := c.attachments("--conversation", conv)
		if len(byConv) != 4 {
			t.Fatalf("list --conversation = %+v, want four attachments", byConv)
		}
		for _, a := range byConv {
			if a.Ref == invoiceRef && len(a.Messages) != 2 {
				t.Errorf("invoice held by %v, want m1 and m2 (%s, %s)", a.Messages, m1.Ref.ID, m2.Ref.ID)
			}
		}
		r := c.heo(0, "attachment", "list", "--message", string(m1.Ref.ID))
		for _, want := range []string{"REF", "invoice.txt", "text/plain", "public", invoiceRef, string(m1.Ref.ID)} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text list lacks %q:\n%s", want, r.stdout)
			}
		}
		wantStderr(t, c.heo(2, "attachment", "list"), "USAGE", "--message or --conversation")
		wantStderr(t, c.heo(3, "attachment", "list", "--message", "00000000-0000-7000-8000-000000000000"), "NOT_FOUND")
		if r := c.heo(0, "attachment", "list", "--message", string(m1.Ref.ID), "--format", "json"); strings.Contains(r.stdout, "content") {
			t.Errorf("list carries content:\n%s", r.stdout)
		}

		// Get: the same metadata, one attachment, no bytes.
		var one desk.Attachment
		c.json(&one, "attachment", "get", invoiceRef)
		if one.Ref != invoiceRef || one.Name != "invoice.txt" || one.Digest != got[0].Digest {
			t.Fatalf("get = %+v", one)
		}
		r = c.heo(0, "attachment", "get", invoiceRef)
		for _, want := range []string{"invoice.txt", "text/plain", "sha256:", "public"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text get lacks %q:\n%s", want, r.stdout)
			}
		}
		if strings.Contains(r.stdout+r.stderr, marker) {
			t.Error("get printed the attachment's bytes")
		}
		wantStderr(t, c.heo(2, "attachment", "get", "nope"), "VALIDATION", "<inbox>/<attachment>")
		wantStderr(t, c.heo(3, "attachment", "get", string(m1.Ref.Conversation.Inbox.ID)+"/00000000-0000-7000-8000-000000000000"), "NOT_FOUND")

		// Download: default name in the working directory, owner-only.
		cwd := c.s.path("cwd")
		r = c.heo(0, "attachment", "download", invoiceRef)
		if readFile(t, filepath.Join(cwd, "invoice.txt")) != invoice {
			t.Fatalf("downloaded = %q", readFile(t, filepath.Join(cwd, "invoice.txt")))
		}
		if fi, _ := os.Stat(filepath.Join(cwd, "invoice.txt")); fi.Mode().Perm() != 0o600 {
			t.Errorf("downloaded file mode = %v, want 0600", fi.Mode().Perm())
		}
		for _, want := range []string{"invoice.txt", "text/plain", "path"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("download summary lacks %q:\n%s", want, r.stdout)
			}
		}
		if strings.Contains(r.stdout+r.stderr, marker) {
			t.Error("the summary carries the bytes")
		}

		// Never overwrites without --force: before the bytes move, and the
		// file is untouched. --force replaces it whole.
		wantStderr(t, c.heo(4, "attachment", "download", invoiceRef), "CONFLICT", "invoice.txt exists", "--force")
		wantStderr(t, c.heo(4, "attachment", "download", invoiceRef, "-o", "invoice.txt"), "CONFLICT", "--force")
		if err := os.WriteFile(filepath.Join(cwd, "invoice.txt"), []byte("local edits"), 0o600); err != nil {
			t.Fatal(err)
		}
		wantStderr(t, c.heo(4, "attachment", "download", invoiceRef), "CONFLICT")
		if readFile(t, filepath.Join(cwd, "invoice.txt")) != "local edits" {
			t.Fatal("a refused download overwrote the file")
		}
		c.heo(0, "attachment", "download", invoiceRef, "--force")
		if readFile(t, filepath.Join(cwd, "invoice.txt")) != invoice {
			t.Fatal("--force did not restore the bytes")
		}

		// -o: a file, a directory (the provider's name is only its last
		// element: ../../evil/photo.bin lands as photo.bin), and --format
		// json with -o keeps the file whole and reports on stdout.
		out := filepath.Join(cwd, "copy.bin")
		c.heo(0, "attachment", "download", photoRef, "-o", out)
		if readFile(t, out) != string(binary) {
			t.Fatalf("-o file = %q", readFile(t, out))
		}
		dir := filepath.Join(cwd, "downloads")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		c.heo(0, "attachment", "download", photoRef, "-o", dir)
		if readFile(t, filepath.Join(dir, "photo.bin")) != string(binary) {
			t.Fatalf("-o dir: %v", blobFiles(t, dir))
		}
		if _, err := os.Stat(filepath.Join(c.s.root, "evil")); err == nil {
			t.Fatal("the provider's path escaped the download directory")
		}
		var sum struct {
			Attachment, Name, Path string
			Size                   int64
		}
		c.json(&sum, "attachment", "download", photoRef, "-o", out, "--force")
		if readFile(t, out) != string(binary) || sum.Attachment != photoRef || sum.Path != out || sum.Size != int64(len(binary)) {
			t.Fatalf("download --format json -o = %+v, file %q", sum, readFile(t, out))
		}

		// -o -: the bytes alone on stdout, exactly; the note on stderr.
		r = c.heo(0, "attachment", "download", photoRef, "-o", "-")
		if r.stdout != string(binary) || !strings.Contains(r.stderr, "to stdout") {
			t.Fatalf("-o - stdout %q, stderr %q", r.stdout, r.stderr)
		}
		r = c.heo(0, "attachment", "download", invoiceRef, "-o", "-", "--format", "json")
		if r.stdout != invoice {
			t.Fatalf("-o - with --format json: stdout %q", r.stdout)
		}

		// The bytes reached no log: not the daemon's, not a summary.
		if strings.Contains(d.log(), marker) {
			t.Errorf("the daemon logged attachment bytes:\n%s", d.log())
		}

		// Narrowing: the provider makes m1 private. The desk still reads
		// it; an external participant, who read it a moment ago, is denied.
		c.stop(d)
		c.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [external]\n")
		d = c.start()
		c.heo(0, "attachment", "get", invoiceRef)
		c.heo(0, "attachment", "download", publicRef, "-o", "-")
		c.stop(d)
		c.s.write("cfg/heo/profiles/default.yaml", "")
		d = c.start()
		c.inject(ch, adapterstest.Inject{
			Event: "m1-private", Message: "m1", Kind: model.EnvelopeMessageEdited, From: "ada", Destination: "files-1",
			Audience: model.Audience{Visibility: model.Internal}, OccurredAt: time.Now().Add(time.Hour),
		})
		c.inject(ch, adapterstest.Inject{
			Event: "m2-private", Message: "m2", Kind: model.EnvelopeMessageEdited, From: "ada", Destination: "files-1",
			Audience: model.Audience{Visibility: model.Internal}, OccurredAt: time.Now().Add(time.Hour),
		})
		if r := c.heo(0, "attachment", "download", invoiceRef, "-o", "-"); r.stdout != invoice {
			t.Fatalf("desk download after narrowing: %q", r.stdout)
		}
		c.stop(d)

		c.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [external]\n")
		d = c.start()
		for _, args := range [][]string{
			{"attachment", "get", invoiceRef},
			{"attachment", "get", photoRef},
			{"attachment", "download", invoiceRef, "-o", "-"},
			{"attachment", "download", photoRef, "-o", filepath.Join(cwd, "denied.bin")},
		} {
			r := c.s.run(nil, args...)
			c.s.want(r, 5, args...)
			wantStderr(t, r, "UNAUTHORIZED")
			if r.stdout != "" || strings.Contains(r.stderr, "gone") || strings.Contains(r.stderr, marker) {
				t.Errorf("%v: stdout %q stderr %q", args, r.stdout, r.stderr)
			}
		}
		if _, err := os.Stat(filepath.Join(cwd, "denied.bin")); err == nil {
			t.Error("a denied download left a file")
		}
		// The narrowed messages are not there for them, as for `message
		// get`; the conversation lists what is left.
		wantStderr(t, c.heo(3, "attachment", "list", "--message", string(m1.Ref.ID)), "NOT_FOUND")
		if got := c.attachments("--conversation", conv); len(got) != 2 || got[0].Ref == invoiceRef || got[1].Ref == invoiceRef || got[0].Ref == photoRef || got[1].Ref == photoRef {
			t.Errorf("external lists %+v, want only what stayed public", got)
		}
		// What stayed public is still theirs.
		if r := c.heo(0, "attachment", "download", publicRef, "-o", "-"); r.stdout != "public bytes" {
			t.Fatalf("external download of a public attachment: %q", r.stdout)
		}
		c.stop(d)

		// A role that does not read is refused by heo's own policy first.
		c.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [operator]\n")
		d = c.start()
		for _, args := range [][]string{
			{"attachment", "list", "--message", string(public.Ref.ID)},
			{"attachment", "get", publicRef},
			{"attachment", "download", publicRef, "-o", "-"},
		} {
			wantDenied(t, c.s.run(nil, args...), "heo.authz.not_granted")
		}
		c.stop(d)

		// Redaction: the bytes are removed and the attachment is gone.
		c.s.write("cfg/heo/profiles/default.yaml", "")
		d = c.start()
		before := blobCount(t, blobDir)
		op := c.db.operator("default")
		cur, err := op.Messages().Get(t.Context(), model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "e2e"}, Ref: doomed.Ref})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := op.Privacy().Redact(t.Context(), model.Redact{
			Command: model.Command{Call: model.Call{Correlation: "e2e"}, Operation: "redact-doomed", ExpectedRevision: cur.Revision},
			Target:  doomed.Ref.Ref(), Message: &doomed.Ref, Reason: "e2e",
		}); err != nil {
			t.Fatalf("redact: %v", err)
		}
		if after := blobCount(t, blobDir); after != before-1 {
			t.Errorf("blob files %d → %d, want the redacted attachment's bytes removed", before, after)
		}
		r = c.heo(3, "attachment", "download", doomedRef, "-o", "-")
		wantStderr(t, r, "NOT_FOUND", "gone")
		if r.stdout != "" {
			t.Errorf("a gone attachment wrote %q", r.stdout)
		}
		wantStderr(t, c.heo(3, "attachment", "get", doomedRef), "NOT_FOUND", "gone")
		// The tombstone holds nothing.
		if got := c.attachments("--message", string(doomed.Ref.ID)); len(got) != 0 {
			t.Errorf("redacted message lists %+v", got)
		}
		if got := c.attachments("--conversation", conv); len(got) != 3 {
			t.Errorf("conversation lists %d attachments after the redaction, want 3", len(got))
		}
		c.stop(d)
		if strings.Contains(d.log(), marker) {
			t.Errorf("the daemon logged attachment bytes:\n%s", d.log())
		}
	})
}

// TestAttachmentDownloadOverREST: a served surface has no terminal to
// write a file to, and the daemon's host is not where the caller is: it
// returns the bytes in the answer (base64 in JSON) and writes no file on
// the daemon's side, whatever the daemon's working directory holds.
func TestAttachmentDownloadOverREST(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		ch := c.fakeChannel("fake", adapterstest.Config{Account: "acct-rest"})
		c.heo(0, "inbox", "create", "--channel", "fake", "--key", "files", "--destination", "files-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		payload := []byte{0x00, 0xff, 'r', 'e', 's', 't', 0x10}
		m := c.received(ch, "m1", adapterstest.InjectAttachment{Key: "k", Name: "rest.bin", MediaType: "application/octet-stream", Data: payload})
		ref := attachmentRef(m, 0)
		c.stop(d)

		auth := c.s.useHTTPAuth()
		api := freeAddr(t)
		d = c.s.start(nil, nil, []string{"socket", "workers", "receivers", "api"}, "-c", auth.config,
			"service", "start", "--enable", "api", "--addr", api)
		defer func() { c.s.want(result{exit: d.term()}, 0) }()

		resp, err := authed(http.MethodGet, "http://"+api+"/v1/commands/attachment/download?arg="+url.QueryEscape(ref), auth.token(me()), "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		var env struct {
			Data struct {
				Attachment desk.Attachment
				Content    []byte
			}
		}
		if err := json.Unmarshal(body, &env); resp.StatusCode != http.StatusOK || err != nil {
			t.Fatalf("REST download = %d %v: %s", resp.StatusCode, err, body)
		}
		got := env.Data
		if got.Attachment.Ref != ref || got.Attachment.Name != "rest.bin" || !bytes.Equal(got.Content, payload) {
			t.Fatalf("REST download = %+v (%s)", got, body)
		}
		if entries, _ := os.ReadDir(c.s.path("cwd")); len(entries) != 0 {
			t.Fatalf("files in the daemon's working directory: %v", entries)
		}
	})
}
