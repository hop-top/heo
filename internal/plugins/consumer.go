package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
)

// ConsumerPrefix starts the name of every plugin's subscription in
// uma. Names without it belong to the runtime and the CLI.
const ConsumerPrefix = "plugin-"

// canonicalFilter is the filter as it is hashed: every list sorted and
// de-duplicated, empty as [], keys in this (alphabetical) order.
type canonicalFilter struct {
	Channels []string `json:"channels"`
	Inboxes  []string `json:"inboxes"`
	Kinds    []string `json:"kinds"`
}

// Canonical returns f with each list sorted and de-duplicated; empty
// lists are empty, not nil.
func (f Filter) Canonical() Filter {
	norm := func(l []string) []string {
		out := slices.Clone(l)
		slices.Sort(out)
		out = slices.Compact(out)
		if out == nil {
			out = []string{}
		}
		return out
	}
	return Filter{Kinds: norm(f.Kinds), Channels: norm(f.Channels), Inboxes: norm(f.Inboxes)}
}

// CanonicalJSON is f as the canonical JSON its consumer name hashes,
// for example {"channels":[],"inboxes":["support"],"kinds":["message.received"]}.
func (f Filter) CanonicalJSON() string {
	c := f.Canonical()
	b, _ := json.Marshal(canonicalFilter{Channels: c.Channels, Inboxes: c.Inboxes, Kinds: c.Kinds})
	return string(b)
}

// ConsumerName is the name of the plugin's durable subscription:
// plugin-<name>-<12 hex digits of the SHA-256 of the canonical filter>.
// uma fixes a subscription's filter when it opens it, so a changed
// filter is a new name, and an unchanged one, reordered or repeated
// entries included, is the same.
func ConsumerName(name string, f Filter) string {
	sum := sha256.Sum256([]byte(f.CanonicalJSON()))
	return ConsumerPrefix + name + "-" + hex.EncodeToString(sum[:])[:12]
}

// Consumer is the name of p's subscription.
func (p Plugin) Consumer() string { return ConsumerName(p.Name, p.Filter) }
