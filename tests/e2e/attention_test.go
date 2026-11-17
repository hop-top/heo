package e2e

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// attentionPageJSON is a page of `attention list --format json`.
type attentionPageJSON struct {
	Items []struct {
		Kind         string
		At           time.Time
		Inbox        string
		Conversation *struct{ Ref struct{ ID string } }
		Held         []struct{ ID, Kind string }
	}
	Next     string
	Complete bool
}

// keys names the rows of a page: c:<conversation> or o:<operation>, and
// for a conversation row the operations held on it after a slash.
func (p attentionPageJSON) keys() []string {
	var out []string
	for _, it := range p.Items {
		if it.Conversation == nil {
			out = append(out, "o:"+it.Held[0].ID)
			continue
		}
		k := "c:" + it.Conversation.Ref.ID
		for _, h := range it.Held {
			k += "/" + h.ID
		}
		out = append(out, k)
	}
	return out
}

// attentionOf decodes the page a surface answered, whether it wraps it
// in data or not.
func attentionOf(t *testing.T, surf string, r surfaceResult) attentionPageJSON {
	t.Helper()
	if r.Code != http.StatusOK {
		t.Fatalf("%s attention = %d %s", surf, r.Code, r.Body)
	}
	var wrap struct{ Data json.RawMessage }
	raw := []byte(r.Body)
	if json.Unmarshal(raw, &wrap) == nil && len(wrap.Data) > 0 {
		raw = wrap.Data
	}
	var p attentionPageJSON
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("%s attention: %v: %s", surf, err, r.Body)
	}
	return p
}

// TestAttentionFeedOnEverySurface drives the e2e build on Postgres with
// the hold point on: two inboxes, each with an unread conversation; a
// plugin's message edit is held on one and its label create is held with
// no conversation. The feed lists the label create, then the first inbox's
// conversation with its held edit, then the second inbox's conversation,
// the same on the CLI, REST, gRPC and MCP; one cursor pages it; the inbox
// filter narrows it; a conversation the person has read leaves the feed
// and its held edit stays as a row of its own; a decided operation leaves.
func TestAttentionFeedOnEverySurface(t *testing.T) {
	withE2EBinary(t, func() {
		e := newEffectsSite(t, heldProfile)
		e.stop(e.d)
		auth := e.s.useHTTPAuth()
		api, rpc, mcp := freeAddr(t), freeAddr(t), freeAddr(t)
		d := e.s.start(nil, nil, []string{"socket", "receivers", "api", "rpc", "mcp", "workers"}, "-c", auth.config,
			"service", "start", "--enable", "api", "--enable", "rpc", "--enable", "mcp",
			"--addr", api, "--rpc-addr", rpc, "--mcp-addr", mcp)
		defer func() { e.s.want(result{exit: d.term()}, 0) }()
		x := &surfaces{d: d, auth: auth, api: api, rpc: rpc, mcp: mcp}
		person, plugin := auth.token(me()), auth.token("plugin:triage")
		fxConv, plConv := string(e.fxMsg.Ref.Conversation.ID), string(e.plMsg.Ref.Conversation.ID)
		time.Sleep(20 * time.Millisecond)

		// The plugin proposes a message edit on the first inbox's
		// conversation and a label create: both are held.
		if code, out := x.rest(t, http.MethodPost, "/v1/commands/message/edit", plugin,
			`{"args":["`+string(e.fxMsg.Ref.ID)+`"],"flags":{"revision":"`+string(e.message(e.fxMsg).Revision)+`","text":"edited","idempotency-key":"at-1"}}`); code != http.StatusOK {
			t.Fatalf("plugin edit = %d %v", code, out)
		}
		time.Sleep(20 * time.Millisecond)
		if code, out := x.rest(t, http.MethodPost, "/v1/commands/label/create", plugin,
			`{"args":["billing"],"flags":{"idempotency-key":"at-2"}}`); code != http.StatusOK {
			t.Fatalf("plugin label create = %d %v", code, out)
		}
		var held pageJSON[struct{ ID, Kind string }]
		e.json(&held, "operation", "held", "list")
		var edit, label string
		for _, h := range held.Items {
			if strings.HasPrefix(h.Kind, "label.") {
				label = h.ID
			} else {
				edit = h.ID
			}
		}
		if len(held.Items) != 2 || edit == "" || label == "" {
			t.Fatalf("held = %+v, want an edit and a label create", held.Items)
		}

		want := []string{"o:" + label, "c:" + fxConv + "/" + edit, "c:" + plConv}
		var cli attentionPageJSON
		e.json(&cli, "attention", "list")
		if got := cli.keys(); !slices.Equal(got, want) || !cli.Complete {
			t.Fatalf("CLI feed = %v, want %v", got, want)
		}
		path := []string{"attention", "list"}
		for surf, r := range surfaceCall(t, x, person, path, nil, nil) {
			if got := attentionOf(t, surf, r).keys(); !slices.Equal(got, want) {
				t.Errorf("%s feed = %v, want %v", surf, got, want)
			}
		}
		if got := attentionOf(t, "mcp", mcpCall(t, x, person, "attention.list", map[string]any{})).keys(); !slices.Equal(got, want) {
			t.Errorf("mcp feed = %v, want %v", got, want)
		}

		// One cursor pages the merged feed.
		var seen []string
		cursor := ""
		for range 5 {
			args := []string{"attention", "list", "--limit", "1"}
			if cursor != "" {
				args = append(args, "--cursor", cursor)
			}
			var p attentionPageJSON
			e.json(&p, args...)
			seen = append(seen, p.keys()...)
			if p.Complete {
				break
			}
			if cursor = p.Next; cursor == "" {
				t.Fatalf("page %v is incomplete with no cursor", p.keys())
			}
		}
		if !slices.Equal(seen, want) {
			t.Errorf("paged feed = %v, want %v", seen, want)
		}
		// The cursor is accepted on REST and MCP as well.
		first := attentionOf(t, "mcp", mcpCall(t, x, person, "attention.list", map[string]any{"limit": 1}))
		next := attentionOf(t, "mcp", mcpCall(t, x, person, "attention.list", map[string]any{"limit": 1, "cursor": first.Next}))
		if !slices.Equal(append(first.keys(), next.keys()...), want[:2]) {
			t.Errorf("mcp pages = %v then %v", first.keys(), next.keys())
		}

		// The inbox filter.
		var fx attentionPageJSON
		e.json(&fx, "attention", "list", "--inbox", "fxin")
		if got := fx.keys(); !slices.Equal(got, want[1:2]) {
			t.Errorf("--inbox fxin = %v, want %v", got, want[1:2])
		}
		for surf, r := range surfaceCall(t, x, person, path, nil, map[string]any{"inbox": "plin"}) {
			if got := attentionOf(t, surf, r).keys(); !slices.Equal(got, want[2:]) {
				t.Errorf("%s --inbox plin = %v, want %v", surf, got, want[2:])
			}
		}
		if got := attentionOf(t, "mcp", mcpCall(t, x, person, "attention.list", map[string]any{"inbox": "plin"})).keys(); !slices.Equal(got, want[2:]) {
			t.Errorf("mcp --inbox plin = %v, want %v", got, want[2:])
		}
		if r := mcpCall(t, x, person, "attention.list", map[string]any{"inbox": "nope"}); r.Code == http.StatusOK {
			t.Errorf("mcp --inbox nope = %d %s, want a refusal", r.Code, r.Body)
		}

		// Reading the first inbox's conversation takes it off the feed; its
		// held edit stays, as a row of its own.
		time.Sleep(20 * time.Millisecond)
		var mark readMarkOutJSON
		e.json(&mark, "conversation", "mark-read", fxConv)
		var after attentionPageJSON
		e.json(&after, "attention", "list")
		if got, w := after.keys(), []string{"o:" + label, "o:" + edit, "c:" + plConv}; !slices.Equal(got, w) {
			t.Errorf("feed after reading = %v, want %v", got, w)
		}
		// A rejected operation leaves the feed.
		if code, out := x.rest(t, http.MethodPost, "/v1/commands/operation/held/reject", person, `{"args":["`+label+`"]}`); code != http.StatusOK {
			t.Fatalf("reject = %d %v", code, out)
		}
		e.json(&after, "attention", "list")
		if got, w := after.keys(), []string{"o:" + edit, "c:" + plConv}; !slices.Equal(got, w) {
			t.Errorf("feed after a decision = %v, want %v", got, w)
		}
	})
}
