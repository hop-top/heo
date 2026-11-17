package e2e

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	xrr "hop.top/xrr"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/assist"
	"hop.top/uma/model"
)

// The LLM helpers (conversation summarize and triage, message draft) are
// tested against a real model, recorded once and replayed from then on.
//
// The model is local: a server on this machine, named by
// HEO_E2E_LLM_ENDPOINT when recording (`make record-llm`), model
// llmModel. Recording refuses a host that is not on this machine, and a
// model served from the cloud. The conversation is synthetic. Replay
// needs no model: the endpoint is a closed port and the daemon answers
// from the cassette.

const (
	// llmModel is the model every cassette was recorded with.
	llmModel = "ornith:9b"
	// llmEndpointEnv names the local server a recording talks to.
	llmEndpointEnv = "HEO_E2E_LLM_ENDPOINT"
	// llmNote is the text of the internal note of the synthetic
	// conversation, which only a desk actor's prompt may carry.
	llmNote = "duplicate charge confirmed in the payment dashboard; the account carries a fraud-review flag, so do not mention it to the customer"
)

// llmEndpoint is the model server the daemon is configured with: in
// record mode the local server named by the environment, in replay a
// closed address.
func llmEndpoint(t *testing.T) string {
	t.Helper()
	if xrrModeOf(t) != xrr.ModeRecord {
		return "http://" + freeAddr(t)
	}
	ep := os.Getenv(llmEndpointEnv)
	if ep == "" {
		ep = "http://127.0.0.1:11434"
	}
	if strings.HasSuffix(llmModel, ":cloud") {
		t.Fatalf("recording with %s: a cloud model sends the conversation off this machine", llmModel)
	}
	u, err := url.Parse(ep)
	if err != nil || assist.ClassOfHost(u.Host) != assist.Local {
		t.Fatalf("recording against %s: only a model on this machine may be recorded", ep)
	}
	return ep
}

// llmConfig is the profile's llm section for endpoint.
func llmConfig(endpoint string) string {
	return fmt.Sprintf("  all:\n    timeouts:\n      command: 15m\nllm:\n  model: %q\n  endpoint: %q\n  timeout: 200s\n", llmModel, endpoint)
}

// llmSite is a desk with the synthetic conversation: three inbound
// messages' worth of a billing complaint, one reply and one internal
// note, and three labels.
type llmSite struct {
	*cliSite
	d    *daemon
	proc string
	// env, when set, is the environment of the daemon and replaces the
	// cassette of proc.
	env  []string
	conv string
	ch   model.Channel
}

// newLLMSite starts the daemon, recording or replaying its model calls
// under cassette directory proc when proc is set, and seeds the
// conversation.
func newLLMSite(t *testing.T, extraConfig, proc string) *llmSite {
	t.Helper()
	return newLLMSiteEnv(t, extraConfig, proc, nil)
}

func newLLMSiteEnv(t *testing.T, extraConfig, proc string, env []string) *llmSite {
	t.Helper()
	c := newCLISite(t, extraConfig)
	l := &llmSite{cliSite: c, proc: proc, env: env}
	l.d = l.startDaemon()
	l.seed()
	return l
}

// seed configures the channel, inbox and labels and has the synthetic
// customer write.
func (l *llmSite) seed() {
	t, c := l.t, l.cliSite
	l.ch = c.fakeChannel("fake", adapterstest.Config{Account: "acct-llm"})
	c.heo(0, "inbox", "create", "--channel", "fake", "--key", "support", "--destination", "support-1",
		"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
	for _, name := range []string{"billing", "refund", "shipping"} {
		c.heo(0, "label", "create", name)
	}
	in := func(event, text string) {
		c.inject(l.ch, adapterstest.Inject{Event: event, Message: event, From: "ada", Destination: "support-1", Text: text})
	}
	in("m1", "Hi, I was charged twice for my October subscription (invoice INV-1042). Could you refund the duplicate payment?")
	var msgs pageJSON[model.Message]
	c.json(&msgs, "message", "list", "--inbox", "support")
	if len(msgs.Items) != 1 {
		t.Fatalf("inbound messages = %+v", msgs.Items)
	}
	l.conv = string(msgs.Items[0].Ref.Conversation.ID)
	var reply replyJSON
	c.json(&reply, "message", "reply", l.conv, "--text", "Thanks for flagging this. I can see two charges of 29 euros and I am checking with our billing team.", "--idempotency-key", "llm-seed-reply")
	c.waitDelivery(reply.Operation.ID, model.ProviderAccepted, model.Delivered)
	in("m2", "It has been three days and I still have no answer. I am frustrated, please fix this today.")
	c.heo(0, "note", "add", l.conv, "--text", llmNote, "--placement", "local")
}

func (l *llmSite) startDaemon() *daemon {
	l.t.Helper()
	switch {
	case l.env != nil:
		d := l.s.start(l.env, nil, []string{"socket", "receivers", "webhook", "workers"},
			"-c", "services.webhook.addr="+l.hook, "service", "start", "--enable", "webhook")
		l.t.Cleanup(func() {
			if l.t.Failed() {
				l.t.Logf("daemon log:\n%s", d.log())
			}
		})
		return d
	case l.proc != "":
		return l.startXRR(l.proc)
	}
	return l.start()
}

// restartAs stops the daemon and starts it again with the profile's
// authorization section set to authz (YAML), so the same user holds
// other roles.
func (l *llmSite) restartAs(authz string) {
	l.t.Helper()
	l.stop(l.d)
	l.s.write("cfg/heo/profiles/default.yaml", authz)
	l.d = l.startDaemon()
}

// llmCounts is what the conversation holds and what was queued, to show a
// helper changed neither.
type llmCounts struct {
	messages, operations int
	labels               string
}

func (l *llmSite) counts() llmCounts {
	l.t.Helper()
	var msgs pageJSON[model.Message]
	l.json(&msgs, "message", "list", "--conversation", l.conv, "--limit", "100")
	var ops struct{ Items []json.RawMessage }
	l.json(&ops, "delivery", "list", "--limit", "100")
	var conv model.Conversation
	l.json(&conv, "conversation", "get", l.conv)
	return llmCounts{messages: len(msgs.Items), operations: len(ops.Items), labels: fmt.Sprint(conv.Labels) + string(conv.Revision)}
}

type llmMeta struct {
	Provider         string
	Model            string
	Class            string
	MessageCount     int  `json:"message_count"`
	MessagesInPrompt int  `json:"messages_in_prompt"`
	Truncated        bool `json:"truncated"`
	Audience         string
	MachineGenerated bool `json:"machine_generated"`
}

type summaryJSON struct {
	Summary string
	llmMeta
}

type triageJSON struct {
	Category      string
	Priority      string
	Sentiment     string
	Labels        []string
	DroppedLabels []string `json:"dropped_labels"`
	NextAction    string   `json:"next_action"`
	llmMeta
}

func (m llmMeta) wantLocal(t *testing.T, audience string, messages int) {
	t.Helper()
	if m.Provider != "ollama" || m.Model != llmModel || m.Class != "local" || m.Audience != audience ||
		m.MessageCount != messages || m.MessagesInPrompt != messages || m.Truncated || !m.MachineGenerated {
		t.Errorf("call facts = %+v, want local ollama %s, audience %s, %d messages", m, llmModel, audience, messages)
	}
}

// recordedLLM runs fn on the synthetic conversation with the daemon
// replaying (or recording) its model calls under the test's own
// cassette directory. One test is one cassette, so a model that wanders
// off (a thinking model sometimes does) costs a re-recording of one
// test, not of all.
func recordedLLM(t *testing.T, fn func(l *llmSite, before llmCounts)) {
	t.Helper()
	withE2EBinary(t, func() {
		l := newLLMSite(t, llmConfig(llmEndpoint(t)), "daemon")
		before := l.counts()
		if before.messages != 4 {
			t.Fatalf("the synthetic conversation holds %d messages, want 4: %+v", before.messages, before)
		}
		fn(l, before)
		if after := l.counts(); after != before {
			t.Errorf("the helper changed the conversation: before %+v after %+v", before, after)
		}
	})
}

// TestRecordedLLMSummarizeAsDesk: a desk actor's summary is made from
// every message it may read, the internal note included, and says so.
func TestRecordedLLMSummarizeAsDesk(t *testing.T) {
	recordedLLM(t, func(l *llmSite, _ llmCounts) {
		var sum summaryJSON
		l.json(&sum, "conversation", "summarize", l.conv)
		sum.wantLocal(t, "desk", 4)
		if strings.TrimSpace(sum.Summary) == "" {
			t.Errorf("empty summary: %+v", sum)
		}
	})
}

// TestRecordedLLMSummarizeAsViewer: a viewer's summary is made from the
// public messages alone; the same conversation, a different prompt.
func TestRecordedLLMSummarizeAsViewer(t *testing.T) {
	withE2EBinary(t, func() {
		l := newLLMSite(t, llmConfig(llmEndpoint(t)), "daemon")
		l.restartAs("authorization:\n  owner: [viewer]\n")
		var sum summaryJSON
		l.json(&sum, "conversation", "summarize", l.conv)
		sum.wantLocal(t, "public", 3)
		if strings.TrimSpace(sum.Summary) == "" {
			t.Errorf("empty summary: %+v", sum)
		}
	})
}

// TestRecordedLLMTriage: the model's triage is validated against the
// schema, its labels are the tenant's, and nothing is applied: the
// conversation's labels and revision are the same after.
func TestRecordedLLMTriage(t *testing.T) {
	recordedLLM(t, func(l *llmSite, _ llmCounts) {
		var tri triageJSON
		l.json(&tri, "conversation", "triage", l.conv)
		tri.wantLocal(t, "desk", 4)
		if !slices.Contains(assist.Priorities, tri.Priority) || !slices.Contains(assist.Sentiments, tri.Sentiment) ||
			tri.Category == "" || tri.NextAction == "" || tri.Labels == nil || tri.DroppedLabels == nil {
			t.Errorf("triage = %+v", tri)
		}
		for _, name := range tri.Labels {
			if !slices.Contains([]string{"billing", "refund", "shipping"}, name) {
				t.Errorf("suggested label %q is not one of the tenant's", name)
			}
		}
	})
}

// TestRecordedLLMDraft: a draft is made from the public messages, prints
// alone on stdout under --format text, and is neither sent nor queued.
func TestRecordedLLMDraft(t *testing.T) {
	recordedLLM(t, func(l *llmSite, _ llmCounts) {
		r := l.heo(0, "message", "draft", l.conv, "--instruction", "apologise for the delay and say the refund is being processed", "--tone", "friendly", "--format", "text")
		out := strings.TrimSpace(r.stdout)
		if out == "" || strings.Contains(r.stdout, "provider") || strings.Contains(r.stdout, "audience") || strings.Contains(strings.ToLower(r.stdout), "fraud") {
			t.Errorf("--format text must print the draft alone:\n%s", r.stdout)
		}
		if !strings.Contains(r.stderr, "machine-generated") || !strings.Contains(r.stderr, "nothing was sent") {
			t.Errorf("the status lines belong on stderr:\n%s", r.stderr)
		}
		// The draft is text for `message reply`: previewing the reply
		// with it sends nothing either.
		prev := l.heo(0, "message", "reply", l.conv, "--text", out, "--dry-run")
		if !strings.Contains(prev.stdout, "public") {
			t.Errorf("reply preview:\n%s", prev.stdout)
		}
	})
}

// TestLLMCassettesHonourTheAudience reads what was recorded: the internal
// note is in the requests of the desk's summary and triage and in no
// other, the viewer's and the draft's among them.
func TestLLMCassettesHonourTheAudience(t *testing.T) {
	const note = "fraud-review flag"
	want := map[string]bool{
		"TestRecordedLLMSummarizeAsDesk": true, "TestRecordedLLMTriage": true,
		"TestRecordedLLMSummarizeAsViewer": false, "TestRecordedLLMDraft": false,
	}
	for test, carries := range want {
		dir := filepath.Join("testdata", "cassettes", test, "daemon")
		reqs, err := filepath.Glob(filepath.Join(dir, "*.req.yaml"))
		if err != nil || len(reqs) == 0 {
			t.Fatalf("%s: no recorded requests in %s (%v)", test, dir, err)
		}
		for _, f := range reqs {
			b, err := os.ReadFile(f) //nolint:gosec // G304: committed cassettes
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(b), note); got != carries {
				t.Errorf("%s: request mentions the note = %v, want %v", f, got, carries)
			}
		}
	}
}
