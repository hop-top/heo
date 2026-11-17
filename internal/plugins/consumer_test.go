package plugins_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"hop.top/heo/internal/plugins"
)

func TestCanonicalJSON(t *testing.T) {
	cases := []struct {
		name string
		f    plugins.Filter
		want string
	}{
		{"empty", plugins.Filter{}, `{"channels":[],"inboxes":[],"kinds":[]}`},
		{"sorted and de-duplicated", plugins.Filter{
			Kinds:    []string{"message.received", "message.created", "message.received"},
			Channels: []string{"mail", "chat", "mail"},
			Inboxes:  []string{"support", "sales", "support"},
		}, `{"channels":["chat","mail"],"inboxes":["sales","support"],"kinds":["message.created","message.received"]}`},
	}
	for _, c := range cases {
		if got := c.f.CanonicalJSON(); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestConsumerName(t *testing.T) {
	f := plugins.Filter{Kinds: []string{"message.received"}, Inboxes: []string{"support"}}
	sum := sha256.Sum256([]byte(f.CanonicalJSON()))
	want := "plugin-triage-" + hex.EncodeToString(sum[:])[:12]
	if got := plugins.ConsumerName("triage", f); got != want {
		t.Errorf("got %s want %s", got, want)
	}
	// Reordered and repeated entries keep the name.
	same := plugins.Filter{Kinds: []string{"message.received", "message.received"}, Inboxes: []string{"support"}}
	if plugins.ConsumerName("triage", same) != want {
		t.Error("repeated kind changed the consumer name")
	}
	two := plugins.Filter{Inboxes: []string{"b", "a"}}
	rev := plugins.Filter{Inboxes: []string{"a", "b"}}
	if plugins.ConsumerName("p", two) != plugins.ConsumerName("p", rev) {
		t.Error("inbox order changed the consumer name")
	}
}

// Every list is part of the hash: a second inbox, a channel or a kind is
// a different subscription.
func TestConsumerNameCoversEveryList(t *testing.T) {
	base := plugins.Filter{Inboxes: []string{"a"}}
	for name, f := range map[string]plugins.Filter{
		"second inbox": {Inboxes: []string{"a", "b"}},
		"channel":      {Inboxes: []string{"a"}, Channels: []string{"mail"}},
		"kind":         {Inboxes: []string{"a"}, Kinds: []string{"message.created"}},
	} {
		if plugins.ConsumerName("p", f) == plugins.ConsumerName("p", base) {
			t.Errorf("%s did not change the consumer name", name)
		}
	}
	if !strings.HasPrefix(plugins.ConsumerName("p", plugins.Filter{}), "plugin-p-") {
		t.Error("prefix")
	}
}
