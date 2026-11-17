// Package approvechat announces a held operation to a configured chat
// conversation and takes the decision back from it.
//
// The conversation is one of the daemon's own channels (Telegram, Slack,
// Teams). A reply or reaction in it decides the operation as the actor
// the sender's identity is mapped to in configuration, never as the
// sender's display name; the decision goes through the same path as
// `operation held approve|reject` (desk.Service.DecideHeld), so uma's
// rules, heo's authorization and the plugin guard apply unchanged.
//
// Trust boundary: configuring a conversation here makes whatever its
// members can read part of what the announcement shows. The preview is
// read as each mapped member of that conversation and included only when
// every one of them may read the operation; otherwise the announcement
// names the operation and nothing else. Members of the conversation that
// are not mapped cannot be checked and see the announcement: map every
// person in an approval conversation, or keep the conversation private.
package approvechat

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/viper"

	"hop.top/uma/model"
)

// Key is the configuration subtree.
const Key = "held_operations.chat"

// Member maps one chat identity to the actor that decides for it.
type Member struct {
	// Identity is the record ID of the uma identity the provider's
	// sender resolves to (`heo identity list`), never a display name.
	Identity string `mapstructure:"identity" json:"identity" yaml:"identity"`
	// Actor is who the sender decides as. A plugin actor is refused.
	Actor model.ActorID `mapstructure:"actor" json:"actor" yaml:"actor"`
}

// Conversation is one approval conversation.
type Conversation struct {
	Inbox        string   `mapstructure:"inbox" json:"inbox" yaml:"inbox"`
	Conversation string   `mapstructure:"conversation" json:"conversation" yaml:"conversation"`
	Members      []Member `mapstructure:"members" json:"members" yaml:"members"`
	// SlackThreadReplies lets a reply anywhere in the announcement's
	// Slack thread decide as a reply to the announcement. Off by default:
	// Slack has no quote reply, so a thread reply, including one to
	// heo's own answer, cannot be told from an answer to the
	// announcement; `approve #<operation>` and reactions still decide.
	// It changes nothing in other channels.
	SlackThreadReplies bool `mapstructure:"slack_thread_replies" json:"slack_thread_replies,omitempty" yaml:"slack_thread_replies,omitempty"`
}

// Key identifies the conversation.
func (c Conversation) Key() string { return c.Inbox + "/" + c.Conversation }

// Config is the approval chat's configuration.
type Config struct {
	Enabled       bool           `mapstructure:"enabled" json:"enabled" yaml:"enabled"`
	Conversations []Conversation `mapstructure:"conversations" json:"conversations" yaml:"conversations"`
}

// Load reads the configuration from v and validates it. Absent is
// disabled.
func Load(v *viper.Viper) (Config, error) {
	var c Config
	if v.IsSet(Key) {
		if err := v.UnmarshalKey(Key, &c); err != nil {
			return Config{}, fmt.Errorf("%s: %w", Key, err)
		}
	}
	return c, c.Validate()
}

// Validate checks the configuration.
func (c Config) Validate() error {
	var bad []string
	seen := map[string]bool{}
	for i, cv := range c.Conversations {
		at := fmt.Sprintf("conversations[%d]", i)
		if cv.Inbox == "" || cv.Conversation == "" {
			bad = append(bad, at+": inbox and conversation are required")
		}
		if seen[cv.Key()] {
			bad = append(bad, at+": "+cv.Key()+" is listed twice")
		}
		seen[cv.Key()] = true
		ids := map[string]bool{}
		for j, m := range cv.Members {
			mat := fmt.Sprintf("%s.members[%d]", at, j)
			switch {
			case strings.TrimSpace(m.Identity) == "":
				bad = append(bad, mat+".identity: required")
			case ids[m.Identity]:
				bad = append(bad, mat+".identity: "+m.Identity+" is mapped twice")
			}
			ids[m.Identity] = true
			if err := m.Actor.Validate(); err != nil {
				bad = append(bad, mat+".actor: "+err.Error())
			} else if m.Actor.IsPlugin() {
				bad = append(bad, mat+".actor: a plugin actor may not decide a held operation")
			}
		}
		if c.Enabled && len(cv.Members) == 0 {
			bad = append(bad, at+": no members; nobody could decide from it")
		}
	}
	if c.Enabled && len(c.Conversations) == 0 {
		bad = append(bad, "enabled with no conversations")
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		return fmt.Errorf("%s: %s", Key, strings.Join(bad, "; "))
	}
	return nil
}

// actorOf returns the actor identity maps to in cv.
func (cv Conversation) actorOf(identity string) (model.ActorID, bool) {
	for _, m := range cv.Members {
		if m.Identity == identity {
			return m.Actor, true
		}
	}
	return "", false
}
