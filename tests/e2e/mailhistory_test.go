package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"gopkg.in/yaml.v3"

	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// Mail history import through the real heo binary.
//
// Gmail and Graph are replayed from the cassettes uma recorded against
// real mailboxes (testdata/cassettes, copied from uma's own testdata):
// a local server answers each request the adapter makes from the recorded
// response of the same method, path and query, and fails any request no
// cassette holds. The e2e build points the adapters at it
// (HEO_E2E_GMAIL_ENDPOINT, HEO_E2E_GRAPH_ENDPOINT). The recorded window is
// 2001-01-01 to 2001-01-02 UTC, so the imports name it with --since and
// --until: it is also what makes the requests match. One answer is not
// recorded and is served by the test: Graph's $value download of the
// attachment (uma's Graph recording read only the attachment list, as its
// reader runs without a blob store); its bytes are the test's, its size
// the recorded one.
//
// IMAP has no recording: uma's own tests run it against an in-process
// server, and so do these, on go-imap's in-memory server.

const (
	mailMailbox = "mailbox@example.test"
	mailPeer    = "peer@example.test"
)

// cassetteMail replays recorded Gmail or Graph traffic and the OAuth
// token endpoint.
type cassetteMail struct {
	t       *testing.T
	srv     *httptest.Server
	mailbox string
	replace *strings.Replacer
	// root is the API root the recording has before users/ ("/gmail/v1",
	// "/v1.0").
	root    string
	answers map[string]cassetteAnswer
	// extra answers a request the cassettes do not hold, or reports it
	// did not.
	extra func(w http.ResponseWriter, r *http.Request) bool

	mu     sync.Mutex
	served map[string]int
	missed []string
}

type cassetteAnswer struct {
	Status  int
	Headers map[string]string
	Body    string
}

// newCassetteMail loads dir's cassettes and serves them. The recordings
// name the mailbox and the peer by placeholders, which the server
// translates to mailbox and peer, as uma's replay does.
func newCassetteMail(t *testing.T, dir, root, mailbox, peer string) *cassetteMail {
	t.Helper()
	m := &cassetteMail{t: t, mailbox: mailbox, root: root, answers: map[string]cassetteAnswer{}, served: map[string]int{}}
	files, err := filepath.Glob(filepath.Join(dir, "*.req.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no cassettes in %s: %v", dir, err)
	}
	for _, rf := range files {
		var req struct {
			Payload struct{ Method, URL string }
		}
		var resp struct {
			Payload struct {
				Status  int
				Headers map[string]string
				Body    string
			}
		}
		readYAML(t, rf, &req)
		readYAML(t, strings.TrimSuffix(rf, ".req.yaml")+".resp.yaml", &resp)
		u, err := url.Parse(req.Payload.URL)
		if err != nil {
			t.Fatal(err)
		}
		m.answers[cassetteKey(req.Payload.Method, u.Path, u.Query())] = cassetteAnswer{resp.Payload.Status, resp.Payload.Headers, resp.Payload.Body}
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	// A link a recorded answer holds names the provider's own host; it
	// leads here, as the same request would lead to the provider.
	m.replace = strings.NewReplacer("MAILBOX", mailbox, "PEER", peer,
		"https://graph.microsoft.com", m.srv.URL, "https://gmail.googleapis.com", m.srv.URL)
	t.Cleanup(m.srv.Close)
	t.Cleanup(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if len(m.missed) > 0 {
			t.Errorf("requests no cassette answers:\n%s", strings.Join(m.missed, "\n"))
		}
	})
	return m
}

func readYAML(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: a committed cassette
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// cassetteKey is a request as the cassette and the adapter can both
// spell it: the method, the unescaped path and the sorted query.
func cassetteKey(method, path string, q url.Values) string {
	return method + " " + path + "?" + q.Encode()
}

func (m *cassetteMail) endpoint() string { return m.srv.URL + m.root }
func (m *cassetteMail) tokenURL() string { return m.srv.URL + "/token" }

func (m *cassetteMail) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"replay-token","token_type":"Bearer","expires_in":3600}`)
		return
	}
	path := strings.ReplaceAll(r.URL.Path, m.mailbox, "MAILBOX")
	key := cassetteKey(r.Method, path, r.URL.Query())
	a, ok := m.answers[key]
	if !ok {
		if m.extra != nil && m.extra(w, r) {
			return
		}
		m.mu.Lock()
		m.missed = append(m.missed, key)
		m.mu.Unlock()
		http.Error(w, `{"error":{"code":404,"message":"no cassette"}}`, http.StatusNotFound)
		return
	}
	m.mu.Lock()
	m.served[key]++
	m.mu.Unlock()
	for k, v := range a.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(a.Status)
	_, _ = io.WriteString(w, m.replace.Replace(a.Body))
}

// servedCount is the number of requests answered whose key has substr.
func (m *cassetteMail) servedCount(substr string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, c := range m.served {
		if strings.Contains(k, substr) {
			n += c
		}
	}
	return n
}

// mailSite is a daemon with mail channels and no receivers: the import
// is the only thing that reads the provider.
type mailSite struct {
	*cliSite
	d *daemon
}

// newMailSite starts the e2e build's daemon with env set, without its
// receivers and workers, so only `history fetch` reaches the provider.
func newMailSite(t *testing.T, env ...string) *mailSite {
	t.Helper()
	c := newCLISite(t, "")
	args := []string{"-c", "services.webhook.addr=" + c.hook, "service", "start", "--enable", "webhook", "--disable=receivers,workers"}
	d := c.s.start(env, nil, []string{"socket", "webhook"}, args...)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", d.log())
		}
	})
	return &mailSite{cliSite: c, d: d}
}

// refreshGrant stores a refresh-token grant under ref, against tokenURL,
// as a mailbox with a consent would have.
func (c *cliSite) refreshGrant(ref, tokenURL string) {
	c.t.Helper()
	grant, _ := json.Marshal(map[string]any{
		"v": 1, "kind": "refresh_token", "token_url": tokenURL, "client_id": ref,
		"scopes": []string{"mail"}, "secret_ref": "uma/default/" + ref + "-rt",
	})
	rt, _ := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": "rt-" + ref})
	c.s.write("secrets/uma/default/"+ref, string(grant))
	c.s.write("secrets/uma/default/"+ref+"-rt", string(rt))
}

func (c *cliSite) createChannel(typ model.TypeKey, key, cfg, credential string) model.Channel {
	c.t.Helper()
	var out channelOutJSON
	c.json(&out, "channel", "create", "--type", string(typ), "--key", key, "--configuration", cfg, "--credential-ref", credential)
	return out.Channel
}

// mailTexts is every message text of every conversation of the inbox, as
// "subject-ish text", and the conversation count.
func (c *cliSite) mailTexts(inbox string) []string {
	c.t.Helper()
	var out []string
	for _, cv := range c.conversationsOf(inbox) {
		var msgs pageJSON[model.Message]
		c.json(&msgs, "message", "list", "--conversation", string(cv.Ref.ID), "--limit", "100")
		for _, m := range msgs.Items {
			out = append(out, strings.TrimSpace(desk.Text(m.Content)))
		}
	}
	return out
}

// mailInbox gives the channel the inbox its mail is bound to, by the
// address it is received at, one conversation per sender.
func (c *cliSite) mailInbox(channel, destination string) {
	c.t.Helper()
	c.heo(0, "inbox", "create", "--channel", channel, "--key", "mail", "--destination", destination, "--mode", model.ModePerContact,
		"--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
}

// historyOf runs `history fetch` with args and decodes its report.
func (c *cliSite) historyOf(args ...string) mailHistoryJSON {
	c.t.Helper()
	var h mailHistoryJSON
	c.json(&h, append([]string{"history", "fetch"}, args...)...)
	return h
}

type mailHistoryJSON struct {
	historyJSON
	Until string
}

// rawAttachment is the first attachment of the raw RFC 822 message a Gmail
// cassette holds in its "raw" field: its file name and bytes.
func rawAttachment(t *testing.T, cassetteBody string) (name string, data []byte) {
	t.Helper()
	var body struct{ Raw string }
	if err := json.Unmarshal([]byte(cassetteBody), &body); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.URLEncoding.DecodeString(body.Raw)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(body.Raw)
	}
	if err != nil {
		t.Fatal(err)
	}
	return mimeAttachment(t, raw)
}

// mimeAttachment is the first part of raw that is a named attachment.
func mimeAttachment(t *testing.T, raw []byte) (string, []byte) {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("no attachment in the message: %v", err)
		}
		if p.FileName() == "" {
			continue
		}
		b, err := io.ReadAll(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.EqualFold(p.Header.Get("Content-Transfer-Encoding"), "base64") {
			if b, err = base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(b)), "")); err != nil {
				t.Fatal(err)
			}
		}
		return p.FileName(), b
	}
}

// downloaded is attachment ref's bytes, through `attachment download -o -`.
func (c *cliSite) downloaded(ref string) string {
	c.t.Helper()
	return c.heo(0, "attachment", "download", ref, "-o", "-").stdout
}

// attachmentsOfInbox lists every attachment of every conversation in inbox.
func (c *cliSite) attachmentsOfInbox(inbox string) []desk.Attachment {
	c.t.Helper()
	var out []desk.Attachment
	for _, cv := range c.conversationsOf(inbox) {
		out = append(out, c.attachments("--conversation", string(cv.Ref.ID))...)
	}
	return out
}

func mailImportFlags(since, until string, extra ...string) []string {
	return append([]string{"--since", since, "--until", until}, extra...)
}

// TestMailHistoryGmailCassettes: `history fetch --channel` on a google.mail
// channel, against uma's recorded Gmail mailbox (three messages in the
// window 2001-01-01, the second with a text attachment). The window is
// read oldest first in pages of two with a cursor that resumes it, every
// message once, the attachment's bytes staged and downloadable, a rerun
// imports nothing, and the preview reads nothing.
func TestMailHistoryGmailCassettes(t *testing.T) {
	withE2EBinary(t, func() {
		g := newCassetteMail(t, "testdata/cassettes/TestMailHistoryGmailCassettes", "/gmail/v1", mailMailbox, mailPeer)
		s := newMailSite(t, "HEO_E2E_GMAIL_ENDPOINT="+g.endpoint())
		s.refreshGrant("gm", g.tokenURL())
		cfg, _ := json.Marshal(map[string]any{"user": mailMailbox, "addresses": []string{mailMailbox}, "message_id_domain": "msgid.uma.invalid"})
		s.createChannel(model.TypeGoogleMail, "gm", string(cfg), "gm")
		s.mailInbox("gm", mailMailbox)
		const since, until = "2001-01-01", "2001-01-02"

		var plan planJSON
		s.json(&plan, append([]string{"history", "fetch", "--channel", "gm", "--dry-run"}, mailImportFlags(since, until)...)...)
		var capLine string
		_ = json.Unmarshal(plan.Args["capability"], &capLine)
		if !strings.Contains(capLine, "history declared by google.mail") || string(plan.Args["until"]) != `"2001-01-02T00:00:00Z"` {
			t.Fatalf("preview = %+v capability %q", plan, capLine)
		}
		if n := g.servedCount("/messages"); n != 0 {
			t.Fatalf("--dry-run read the mailbox %d times", n)
		}

		fetch := append([]string{"--channel", "gm"}, mailImportFlags(since, until)...)
		first := s.historyOf(append(fetch, "--limit", "2")...)
		if first.Imported != 2 || first.Complete || first.Next == "" || first.Gap || first.Until != "2001-01-02T00:00:00Z" {
			t.Fatalf("first page = %+v", first)
		}
		second := s.historyOf(append(fetch, "--limit", "2", "--cursor", first.Next)...)
		if second.Imported != 1 || !second.Complete || second.Next != "" || second.Gap {
			t.Fatalf("resumed page = %+v", second)
		}
		again := s.historyOf(append(fetch, "--limit", "500")...)
		if again.Imported != 0 || again.Skipped != 3 || !again.Complete {
			t.Fatalf("a rerun = %+v, want 0 imported and 3 skipped", again)
		}

		const inbox = "mail"
		got := s.mailTexts(inbox)
		for _, want := range []string{"First message of the uma history recording window.", "Second message, with a small text attachment.", "Third"} {
			if !containsText(got, want) {
				t.Errorf("imported mail lacks %q: %q", want, got)
			}
		}
		if len(got) != 3 {
			t.Errorf("imported %d messages, want 3: %q", len(got), got)
		}

		// The attachment's bytes were staged with the import.
		var cassette cassetteAnswer
		for k, a := range g.answers {
			if strings.Contains(k, "e3c9cc8526ed2cf") && strings.Contains(k, "format=raw") {
				cassette = a
			}
		}
		name, want := rawAttachment(t, cassette.Body)
		atts := s.attachmentsOfInbox(inbox)
		if len(atts) != 1 || atts[0].Name != name || atts[0].Size != int64(len(want)) {
			t.Fatalf("attachments = %+v, want %s of %d bytes", atts, name, len(want))
		}
		if bytesGot := s.downloaded(atts[0].Ref); bytesGot != string(want) {
			t.Fatalf("downloaded attachment = %q, want %q", bytesGot, want)
		}

		// A conversation import previews on the imported conversation; what
		// it reads is the sender's mail, which no cassette holds.
		cv := s.conversationsOf(inbox)[0]
		var cplan planJSON
		s.json(&cplan, "history", "fetch", "--conversation", string(cv.Ref.ID), "--dry-run")
		if string(cplan.Args["conversation"]) == "" {
			t.Errorf("conversation preview = %+v", cplan)
		}
		s.stop(s.d)
	})
}

// TestMailHistoryGraphCassettes: the same on a microsoft.mail channel,
// against uma's recorded Graph mailbox (three messages in the window, the
// second with a file attachment, read two at a time).
func TestMailHistoryGraphCassettes(t *testing.T) {
	withE2EBinary(t, func() {
		g := newCassetteMail(t, "testdata/cassettes/TestMailHistoryGraphCassettes", "/v1.0", mailMailbox, mailPeer)
		const attachmentSize = 197
		attachment := strings.Repeat("uma history recording attachment\n", 6)[:attachmentSize]
		g.extra = func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/$value") && strings.Contains(r.URL.Path, "/attachments/") {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, attachment)
				return true
			}
			return false
		}
		s := newMailSite(t, "HEO_E2E_GRAPH_ENDPOINT="+g.endpoint())
		s.refreshGrant("ms", g.tokenURL())
		cfg, _ := json.Marshal(map[string]any{"mailbox": mailMailbox, "addresses": []string{mailMailbox}})
		s.createChannel(model.TypeMicrosoftMail, "ms", string(cfg), "ms")
		s.mailInbox("ms", mailMailbox)
		fetch := append([]string{"--channel", "ms"}, mailImportFlags("2001-01-01", "2001-01-02")...)

		first := s.historyOf(append(fetch, "--limit", "2")...)
		if first.Imported != 2 || first.Complete || first.Next == "" || first.Gap {
			t.Fatalf("first page = %+v", first)
		}
		second := s.historyOf(append(fetch, "--limit", "2", "--cursor", first.Next)...)
		if second.Imported != 1 || !second.Complete || second.Gap {
			t.Fatalf("resumed page = %+v", second)
		}
		// The recorded pages are two wide, so a rerun walks them again.
		var imported, skipped int
		for cur, n := "", 0; ; n++ {
			if n > 5 {
				t.Fatal("the cursor never ends")
			}
			args := append(slices.Clone(fetch), "--limit", "2")
			if cur != "" {
				args = append(args, "--cursor", cur)
			}
			r := s.historyOf(args...)
			imported, skipped = imported+r.Imported, skipped+r.Skipped
			if cur = r.Next; cur == "" {
				break
			}
		}
		if imported != 0 || skipped != 3 {
			t.Fatalf("a rerun imported %d and skipped %d, want 0 and 3", imported, skipped)
		}

		const inbox = "mail"
		got := s.mailTexts(inbox)
		for _, want := range []string{"First message of the uma history recording window.", "Second message, with a small text attachment.", "Third and last message of the window."} {
			if !containsText(got, want) {
				t.Errorf("imported mail lacks %q: %q", want, got)
			}
		}
		atts := s.attachmentsOfInbox(inbox)
		if len(atts) != 1 || atts[0].Name != "notes.txt" || atts[0].Size != attachmentSize {
			t.Fatalf("attachments = %+v, want notes.txt of %d bytes", atts, attachmentSize)
		}
		if b := s.downloaded(atts[0].Ref); b != attachment {
			t.Fatalf("downloaded attachment = %q, want %q", b, attachment)
		}
		s.stop(s.d)
	})
}

func containsText(texts []string, prefix string) bool {
	for _, t := range texts {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

// imapFake is an in-process IMAP server on go-imap's in-memory server,
// plaintext on a loopback port, with one user.
type imapFake struct {
	user *imapmemserver.User
	port int
}

const (
	imapUser = "mailbox"
	imapPass = "e2e-imap-password" // #nosec G101 -- test value
)

func newIMAPFake(t *testing.T, mailboxes ...string) *imapFake {
	t.Helper()
	u := imapmemserver.NewUser(imapUser, imapPass)
	for _, m := range append([]string{"INBOX"}, mailboxes...) {
		if err := u.Create(m, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem := imapmemserver.New()
	mem.AddUser(u)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         goimap.CapSet{goimap.CapIMAP4rev1: {}},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = ln.Close()
		<-served
		_ = srv.Close()
	})
	return &imapFake{user: u, port: ln.Addr().(*net.TCPAddr).Port}
}

type literal struct {
	*bytes.Reader
}

func (l literal) Size() int64 { return int64(l.Len()) }

// deliver puts raw in mailbox as received at at.
func (f *imapFake) deliver(t *testing.T, mailbox, raw string, at string) {
	t.Helper()
	when, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.user.Append(mailbox, literal{bytes.NewReader([]byte(raw))}, &goimap.AppendOptions{Time: when}); err != nil {
		t.Fatalf("append to %s: %v", mailbox, err)
	}
}

// imapMessage is a plain message from from, with an optional text
// attachment.
func imapMessage(from, id, subject, body, attName, attBody string) string {
	h := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: Mon, 01 Jan 2001 10:00:00 +0000\r\nMessage-ID: <%s@peers.test>\r\nMIME-Version: 1.0\r\n", from, mailMailbox, subject, id)
	if attName == "" {
		return h + "Content-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n"
	}
	return h + "Content-Type: multipart/mixed; boundary=\"b1\"\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n" +
		"--b1\r\nContent-Type: text/plain; name=\"" + attName + "\"\r\nContent-Disposition: attachment; filename=\"" + attName + "\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte(attBody)) + "\r\n--b1--\r\n"
}

// TestMailHistoryIMAP: `history fetch` on an imap.mail channel against an
// in-process IMAP server. The window (--since, --until) is the server's
// arrival time, the cursor resumes a window longer than the limit, the
// attachment's bytes arrive with the message, a conversation import reads
// its sender's mail only, and the mailboxes read are the channel's
// history_mailboxes: mail in another folder is not imported until the
// channel names the folder.
func TestMailHistoryIMAP(t *testing.T) {
	withE2EBinary(t, func() {
		f := newIMAPFake(t, "Archive", "Sent")
		s := newMailSite(t)
		s.s.write("secrets/uma/default/imap", fmt.Sprintf(`{"v":1,"kind":"password","user":%q,"password":%q}`, imapUser, imapPass))
		server := map[string]any{"host": "127.0.0.1", "plain_port": f.port, "allow_plaintext_loopback": true}
		imapCfg := func(extra map[string]any) string {
			cfg := map[string]any{"address": mailMailbox, "imap": server, "submission": server}
			for k, v := range extra {
				cfg[k] = v
			}
			b, _ := json.Marshal(cfg)
			return string(b)
		}
		s.createChannel(model.TypeImapMail, "im", imapCfg(nil), "imap")
		s.mailInbox("im", mailMailbox)

		const note = "ada's notes, kept as bytes\n"
		f.deliver(t, "INBOX", imapMessage("ada@peers.test", "a1", "Ada one", "ada first", "", ""), "2026-01-01T09:00:00Z")
		f.deliver(t, "INBOX", imapMessage("ada@peers.test", "a2", "Ada two", "ada second", "notes.txt", note), "2026-02-01T09:00:00Z")
		f.deliver(t, "INBOX", imapMessage("cy@peers.test", "c1", "Cy one", "cy first", "", ""), "2026-03-01T09:00:00Z")
		f.deliver(t, "INBOX", imapMessage("ada@peers.test", "a3", "Ada three", "ada third", "", ""), "2026-04-01T09:00:00Z")
		f.deliver(t, "Archive", imapMessage("ada@peers.test", "x1", "Ada archived", "ada in the archive", "", ""), "2026-01-15T09:00:00Z")
		f.deliver(t, "Sent", imapMessage("mailbox@example.test", "s1", "Sent by us", "our reply", "", ""), "2026-01-20T09:00:00Z")

		// A window: arrival times from February to the start of April.
		win := s.historyOf("--channel", "im", "--since", "2026-02-01", "--until", "2026-04-01")
		if win.Imported != 2 || !win.Complete || win.Gap || win.Until != "2026-04-01T00:00:00Z" {
			t.Fatalf("window import = %+v, want ada two and cy one", win)
		}
		const inbox = "mail"
		got := s.mailTexts(inbox)
		if len(got) != 2 || !containsText(got, "ada second") || !containsText(got, "cy first") {
			t.Fatalf("the window imported %q", got)
		}
		wantStderr(t, s.heo(2, "history", "fetch", "--channel", "im", "--since", "2026-04-01", "--until", "2026-03-01"), "USAGE", "--until must be after --since")

		// The attachment arrived with its bytes.
		atts := s.attachmentsOfInbox(inbox)
		if len(atts) != 1 || atts[0].Name != "notes.txt" || atts[0].Size != int64(len(note)) {
			t.Fatalf("attachments = %+v", atts)
		}
		if b := s.downloaded(atts[0].Ref); b != note {
			t.Fatalf("downloaded attachment = %q, want %q", b, note)
		}

		// A conversation reads its sender's mail only: ada's January and April
		// mail (February's is held), not cy's, and not the Archive's.
		var adaConv model.Conversation
		for _, cv := range s.conversationsOf(inbox) {
			var msgs pageJSON[model.Message]
			s.json(&msgs, "message", "list", "--conversation", string(cv.Ref.ID), "--limit", "10")
			if len(msgs.Items) > 0 && strings.HasPrefix(strings.TrimSpace(desk.Text(msgs.Items[0].Content)), "ada second") {
				adaConv = cv
			}
		}
		if adaConv.Ref.ID == "" {
			t.Fatal("ada's conversation not found")
		}
		conv := s.historyOf("--conversation", string(adaConv.Ref.ID))
		if conv.Imported != 2 || conv.Skipped != 1 || !conv.Complete || conv.Conversation != adaConv.Ref.ID {
			t.Fatalf("conversation import = %+v, want ada one and three imported, ada two skipped", conv)
		}

		// Cursors: the rest of the channel, a message at a time.
		p1 := s.historyOf("--channel", "im", "--limit", "1")
		if p1.Complete || p1.Next == "" {
			t.Fatalf("first page of one = %+v", p1)
		}
		var last mailHistoryJSON
		for cur, n := p1.Next, 0; cur != ""; n++ {
			if n > 10 {
				t.Fatal("the cursor never ends")
			}
			last = s.historyOf("--channel", "im", "--limit", "1", "--cursor", cur)
			cur = last.Next
		}
		if !last.Complete || len(s.mailTexts(inbox)) != 4 {
			t.Fatalf("after the cursor walk: %+v, messages %q", last, s.mailTexts(inbox))
		}
		for _, text := range s.mailTexts(inbox) {
			if strings.HasPrefix(text, "ada in the archive") || strings.HasPrefix(text, "our reply") {
				t.Fatalf("a mailbox the channel does not list was read: %q", text)
			}
		}

		// The channel names the Archive: its mail is imported now. Sent mail
		// is refused even when named.
		s.heo(0, "channel", "update", "im", "--revision", string(s.channelRevision("im")), "--configuration", imapCfg(map[string]any{"history_mailboxes": []string{"INBOX", "Archive"}}))
		arch := s.historyOf("--channel", "im")
		if arch.Imported != 1 || arch.Skipped != 4 || !arch.Complete {
			t.Fatalf("import with history_mailboxes = %+v, want the Archive's one", arch)
		}
		if got := s.mailTexts(inbox); !containsText(got, "ada in the archive") || len(got) != 5 {
			t.Fatalf("messages = %q", got)
		}
		r := s.heo(2, "channel", "update", "im", "--revision", string(s.channelRevision("im")), "--configuration", imapCfg(map[string]any{"history_mailboxes": []string{"Sent"}}))
		if !strings.Contains(r.stderr, "history_mailboxes") {
			t.Errorf("a Sent mailbox was not refused by name:\n%s", r.stderr)
		}
		s.stop(s.d)
	})
}

// channelRevision is the current revision of the channel with key.
func (c *cliSite) channelRevision(key string) model.Revision {
	c.t.Helper()
	for _, ch := range c.channels() {
		if ch.Key == key {
			return ch.Revision
		}
	}
	c.t.Fatalf("no channel %s", key)
	return ""
}
