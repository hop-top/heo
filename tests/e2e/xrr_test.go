package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	xrr "hop.top/xrr"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/xrrx"
	"hop.top/uma/model"
)

// Cassette-backed process tests. The parent test decides the mode and
// gives every child process its own cassette directory; the e2e build of
// the binary (tag heoe2e) records or replays what it calls out to:
// provider HTTP and the pg_dump/pg_restore tools (internal/xrrx).
//
// Replay is the default, and a replay that finds no cassette fails the
// test. Record is XRR_MODE=record on the parent (make record); it is
// refused when CI is set. Cassettes live in
// testdata/cassettes/<test>/<process>/ and are committed; the recorded
// peers are fixtures the test owns (a local HTTP provider stand-in, a
// local tool script), never a real account. docs/DEVELOPING.md has the
// procedure.

// xrrWiped holds the cassette directories a record run has cleared, so a
// process started twice under one name keeps what the first one wrote.
var xrrWiped sync.Map

// xrrModeOf is the mode the parent test runs its children in: XRR_MODE,
// replay when unset.
func xrrModeOf(t *testing.T) xrr.Mode {
	t.Helper()
	mode, err := xrrx.ParseMode(os.Getenv(xrr.EnvMode))
	if err != nil {
		t.Fatal(err)
	}
	if mode == xrr.ModeRecord {
		if err := xrrx.CheckRecord(os.Getenv); err != nil {
			t.Fatal(err)
		}
	}
	return mode
}

// xrrEnv is the environment of one child process of the test, named
// proc: its mode, its own cassette directory and a miss log. In replay
// the directory must exist; a miss the child survived fails the test at
// its end. In record the directory is cleared first and audited at the
// end: a recording that carries a credential, a port, a temp path or a
// process id fails the test.
func (s *sandbox) xrrEnv(proc string) []string {
	t := s.t
	t.Helper()
	mode := xrrModeOf(t)
	env := []string{xrr.EnvMode + "=" + string(mode)}
	if mode == xrr.ModePassthrough {
		return env
	}
	dir, err := filepath.Abs(filepath.Join("testdata", "cassettes", filepath.FromSlash(t.Name()), proc))
	if err != nil {
		t.Fatal(err)
	}
	switch mode {
	case xrr.ModeRecord:
		if _, done := xrrWiped.LoadOrStore(dir, true); !done {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			found, err := xrrx.Audit(dir)
			if err != nil {
				t.Errorf("audit %s: %v", dir, err)
			}
			for _, f := range found {
				t.Errorf("recorded cassette is not clean: %s", f)
			}
		})
	case xrr.ModeReplay:
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("no cassettes for %s at %s: record them with `make record` and commit them", proc, dir)
		}
	}
	miss := s.path("xrr-miss-" + proc + ".log")
	t.Cleanup(func() {
		if b, _ := os.ReadFile(miss); len(b) > 0 { //nolint:gosec // G304: the test's own sandbox
			t.Errorf("%s replayed with no cassette for:\n%s", proc, b)
		}
	})
	return append(env, xrr.EnvCassetteDir+"="+dir, xrrx.EnvMissLog+"="+miss)
}

// startXRR is start with the daemon recording or replaying under its own
// cassette directory.
func (c *cliSite) startXRR(proc string) *daemon {
	c.t.Helper()
	args := []string{"-c", "services.webhook.addr=" + c.hook, "service", "start", "--enable", "webhook"}
	d := c.s.start(c.s.xrrEnv(proc), nil, []string{"socket", "receivers", "webhook", "workers"}, args...)
	c.t.Cleanup(func() {
		if c.t.Failed() {
			c.t.Logf("daemon log:\n%s", d.log())
		}
	})
	return d
}

// providerStandIn is the provider a channel sends to: in record mode a
// real HTTP server the test owns, answering every send with a message
// ID; in replay a closed address, so a send that is not answered from a
// cassette fails instead of reaching anything.
type providerStandIn struct {
	url    string
	hits   atomic.Int32
	mu     sync.Mutex
	bodies []adapterstest.SendBody
	auth   []string
}

func newProviderStandIn(t *testing.T, bearer string) *providerStandIn {
	t.Helper()
	p := &providerStandIn{}
	if xrrModeOf(t) == xrr.ModeReplay {
		p.url = "http://" + freeAddr(t)
		return p
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var b adapterstest.SendBody
		_ = json.Unmarshal(raw, &b)
		p.mu.Lock()
		p.bodies = append(p.bodies, b)
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		n := len(p.bodies)
		p.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != adapterstest.SendPath || r.Header.Get("Authorization") != "Bearer "+bearer {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id":"prov-msg-%d"}`, n)
	}))
	t.Cleanup(srv.Close)
	p.url = srv.URL
	return p
}

// providerChannel stores the provider's credential and creates a
// provider-variant channel and an inbox on it.
func (c *cliSite) providerChannel(p *providerStandIn, bearer string) model.Channel {
	c.t.Helper()
	key, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": bearer})
	if err != nil {
		c.t.Fatal(err)
	}
	c.s.write("secrets/uma/default/provider-key", string(key))
	cfg, err := json.Marshal(adapterstest.ProviderConfig{
		Config: adapterstest.Config{Account: "acct-provider"}, URL: p.url})
	if err != nil {
		c.t.Fatal(err)
	}
	var out channelOutJSON
	c.json(&out, "channel", "create", "--type", string(adapterstest.ProviderType), "--key", "prov",
		"--configuration", string(cfg), "--credential-ref", "provider-key")
	c.heo(0, "inbox", "create", "--channel", "prov", "--key", "provin", "--destination", "prov-1",
		"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
	return out.Channel
}

// TestXRRWebhookFedReplyOverHTTP: a channel fed by a webhook answers
// over HTTP to its provider. The provider's push reaches the channel's
// webhook route; a desk reply and a message that starts another
// conversation are sent by the daemon's workers over the daemon's own
// HTTP client to the provider, with the channel's credential as a bearer
// token. Recorded once against a stand-in the test owns, the exchange
// replays with no provider listening (the address is closed), on another
// port, and the cassettes keep no token, port or temp path.
func TestXRRWebhookFedReplyOverHTTP(t *testing.T) {
	withE2EBinary(t, func() {
		const bearer = "provider-secret-token-0123456789"
		c := newCLISite(t, "")
		p := newProviderStandIn(t, bearer)
		d := c.startXRR("daemon")
		ch := c.providerChannel(p, bearer)

		c.inject(ch, adapterstest.Inject{Event: "e1", Message: "m1", From: "ada", Destination: "prov-1", Text: "hello from the provider"})
		var msgs pageJSON[model.Message]
		c.json(&msgs, "message", "list", "--inbox", "provin")
		if len(msgs.Items) != 1 {
			t.Fatalf("inbound messages = %+v", msgs.Items)
		}
		conv := string(msgs.Items[0].Ref.Conversation.ID)

		var reply replyJSON
		c.json(&reply, "message", "reply", conv, "--text", "we are on it", "--idempotency-key", "xrr-reply")
		if got := c.waitDelivery(reply.Operation.ID, model.ProviderAccepted, model.Delivered); got.Failure != nil {
			t.Fatalf("reply delivery = %+v", got)
		}
		var sent sendJSON
		c.json(&sent, "message", "send", "--inbox", "provin", "--to", "user:grace", "--text", "welcome aboard", "--idempotency-key", "xrr-send")
		if got := c.waitDelivery(sent.Operation.ID, model.ProviderAccepted, model.Delivered); got.Failure != nil {
			t.Fatalf("send delivery = %+v", got)
		}

		if xrrModeOf(t) == xrr.ModeReplay {
			if n := p.hits.Load(); n != 0 {
				t.Errorf("the provider was reached %d times in replay", n)
			}
		} else {
			p.mu.Lock()
			defer p.mu.Unlock()
			want := []adapterstest.SendBody{{To: "ada", Text: "we are on it"}, {To: "grace", Text: "welcome aboard"}}
			if len(p.bodies) != 2 || p.bodies[0] != want[0] || p.bodies[1] != want[1] {
				t.Errorf("the provider was sent %+v, want %+v", p.bodies, want)
			}
			for _, a := range p.auth {
				if a != "Bearer "+bearer {
					t.Errorf("provider saw Authorization %q", a)
				}
			}
		}
		c.stop(d)
		if strings.Contains(d.log(), bearer) {
			t.Error("the daemon log carries the provider credential")
		}
	})
}

// pgDumpStub writes a pg_dump the test owns, as a script: it leaves a
// marker file when it runs and then writes archive to stdout, or, for a
// non-empty failure, says it on stderr and exits 3. Its path is what
// storage.postgres.pg_dump names.
func pgDumpStub(s *sandbox, marker, archive, failure string) string {
	s.t.Helper()
	body := fmt.Sprintf("#!/bin/sh\n: > %q\n", marker)
	if failure != "" {
		body += fmt.Sprintf("echo %q >&2\nexit 3\n", failure)
	} else {
		body += fmt.Sprintf("printf %%s %q\n", archive)
	}
	p := s.write("bin/"+filepath.Base(marker)+"/pg_dump", body)
	if err := os.Chmod(p, 0o700); err != nil { //nolint:gosec // G302: an executable stub in the test's sandbox
		s.t.Fatal(err)
	}
	return p
}

// TestXRRStorageBackupThroughPgDump: `storage backup` runs the
// configured pg_dump as a child of the CLI process. Recorded once with
// a tool the test owns, the run replays without starting the tool (its
// marker never appears), keyed by the tool's name and arguments but not
// the connection string, with the same bytes in the backup archive and,
// for a tool that failed, the same error. The password the tool is given
// travels in its environment and no cassette holds it.
func TestXRRStorageBackupThroughPgDump(t *testing.T) {
	withE2EBinary(t, func() {
		const archive = "PGDMP stub archive, fixed bytes"
		c := newCLISite(t, "")
		backups := c.s.path("backups")
		replay := xrrModeOf(t) == xrr.ModeReplay
		ran := func(marker string) bool {
			_, err := os.Stat(marker)
			return err == nil
		}

		marker := c.s.path("ran-ok")
		tool := pgDumpStub(c.s, marker, archive, "")
		args := []string{"storage", "backup", "--format", "json", "-c", "storage.postgres.pg_dump=" + tool, "-c", "storage.backup.dir=" + backups}
		r := c.s.run(c.s.xrrEnv("backup"), args...)
		c.s.want(r, 0, args...)
		var bk struct{ Path string }
		if err := json.Unmarshal([]byte(r.stdout), &bk); err != nil {
			t.Fatalf("backup output: %v\n%s", err, r.stdout)
		}
		if got := string(archiveEntry(t, bk.Path, "store.dump")); got != archive {
			t.Errorf("store.dump = %q, want %q", got, archive)
		}
		if ran(marker) == replay {
			t.Errorf("tool ran = %v in %s mode", ran(marker), xrrModeOf(t))
		}

		failMarker := c.s.path("ran-failing")
		failing := pgDumpStub(c.s, failMarker, "", "pg_dump: error: the stub refuses")
		args = []string{"storage", "backup", "--format", "json", "-c", "storage.postgres.pg_dump=" + failing, "-c", "storage.backup.dir=" + backups}
		r = c.s.run(c.s.xrrEnv("backup-failing"), args...)
		if r.exit == 0 || !strings.Contains(r.stderr, "the stub refuses") {
			t.Errorf("failing pg_dump: exit %d, stderr %q; want a failure naming the tool's message", r.exit, r.stderr)
		}
		if ran(failMarker) == replay {
			t.Errorf("failing tool ran = %v in %s mode", ran(failMarker), xrrModeOf(t))
		}
	})
}
