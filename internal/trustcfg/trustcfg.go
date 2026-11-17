// Package trustcfg is the trust ladder's configuration and decision
// rules: which verb classes become automatic after how many clean
// approvals, which are always held, the seeded classes of a new person
// and the opt-in supervised first week. It holds no state: the counts
// are uma's, the caller reads them and passes them in.
package trustcfg

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Key is the configuration key of the ladder.
const Key = "held_operations.trust"

// DefaultThreshold is the threshold of a class with none configured.
const DefaultThreshold = 15

// UnsubscribeClass is the class of a conversation unsubscribe. It is
// irreversible, so it has no threshold and is never automatic.
const UnsubscribeClass = "unsubscribe"

// UnsubscribeReason is why an unsubscribe is held: always, whatever the
// count, the seed or the configuration.
const UnsubscribeReason = "unsubscribe is irreversible: always held"

// NeverAutoReasonPrefix starts the reason a class under never_auto is
// held: it is capped at asking in batches, so a batch approval may
// decide it, unlike the always-held rules.
const NeverAutoReasonPrefix = "never automatic:"

// The policies for a first reply to a contact heo has never written to
// on an inbox (held_operations.trust.inboxes.<inbox>.first_reply).
const (
	// FirstReplyHold holds such a reply whatever the trust: the default.
	FirstReplyHold = "hold"
	// FirstReplyAllow lets it follow the ordinary reply:send rules.
	FirstReplyAllow = "allow"
)

// FirstReplyReason is why a first reply to an unknown contact is held.
const FirstReplyReason = "first reply to a contact never written to on this inbox"

// DefaultSupervisedDays is the length of the supervised first week.
const DefaultSupervisedDays = 7

// Seedable are the only classes an organisation may seed: low-risk
// state changes. Every other class earns its trust per person and
// mailbox.
var Seedable = []string{"read", "unread", "archive"}

// Config is the ladder's configuration.
type Config struct {
	// Enabled replaces the hold-everything default with the ladder.
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`
	// Thresholds is the number of consecutive clean approvals after
	// which a class is automatic, per class. Zero means always
	// automatic.
	Thresholds map[string]int `mapstructure:"thresholds" yaml:"thresholds"`
	// NeverAuto lists the classes that never turn automatic, whatever
	// their count: the ceiling is asking in batches. A class names an
	// exact class (trash) or a base (label, covering label:finance).
	NeverAuto []string `mapstructure:"never_auto" yaml:"never_auto"`
	// Seed lists the classes automatic from day one for a person and
	// mailbox with no correction on record. Only Seedable classes.
	Seed []string `mapstructure:"seed" yaml:"seed"`
	// ApprovalAlways lists the mailboxes (inbox IDs) whose every write
	// is held, whatever the trust.
	ApprovalAlways []string `mapstructure:"approval_always" yaml:"approval_always"`
	// Unattended says the plugins run with no human present: a send is
	// then always held.
	Unattended bool `mapstructure:"unattended" yaml:"unattended"`
	// Supervised is the opt-in first week.
	Supervised Supervised `mapstructure:"supervised" yaml:"supervised"`
	// Inboxes holds the policy of single inboxes, keyed by inbox ID
	// (case-insensitive, as the configuration loader folds map keys).
	Inboxes map[string]InboxPolicy `mapstructure:"inboxes" yaml:"inboxes"`
}

// InboxPolicy is the ladder's policy for one inbox.
type InboxPolicy struct {
	// Thresholds override the ladder's, for this inbox: an exact class
	// or its base before ':', ahead of the same names in the ladder's.
	Thresholds map[string]int `mapstructure:"thresholds" yaml:"thresholds"`
	// FirstReply is FirstReplyHold (the default) or FirstReplyAllow.
	FirstReply string `mapstructure:"first_reply" yaml:"first_reply"`
}

// Supervised is the opt-in supervised first week: every class is held
// for Days after a person's start on a mailbox, whatever the trust.
type Supervised struct {
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`
	Days    int  `mapstructure:"days" yaml:"days"`
	// Started maps "<on behalf of>/<mailbox>" to the RFC 3339 time the
	// person started on that mailbox. A pair with no start is held.
	Started map[string]string `mapstructure:"started" yaml:"started"`
}

// DefaultThresholds is the ladder's table, one entry per class.
func DefaultThresholds() map[string]int {
	return map[string]int{
		"read": 3, "unread": 3, "star": 3, "unstar": 3,
		"label": 3, "archive": 3, "move_to": 3,
		"reply:draft": 5, "trash": 5, "spam": 5,
		"reply:send":       8,
		"attention":        0,
		"lane:unsolicited": 5, "lane:newsletter": 3,
	}
}

// Defaults is the configuration of a ladder nobody configured.
func Defaults() Config {
	return Config{Thresholds: DefaultThresholds(), Unattended: true,
		Supervised: Supervised{Days: DefaultSupervisedDays}}
}

// Load reads the ladder from v over Defaults and validates it.
func Load(v *viper.Viper) (Config, error) {
	c := Defaults()
	var raw Config
	raw.Unattended = c.Unattended
	if v.IsSet(Key) {
		if err := v.UnmarshalKey(Key, &raw); err != nil {
			return Config{}, fmt.Errorf("%s: %w", Key, err)
		}
	}
	c.Enabled, c.Seed, c.ApprovalAlways, c.Unattended = raw.Enabled, raw.Seed, raw.ApprovalAlways, raw.Unattended
	for k, n := range raw.Thresholds {
		c.Thresholds[strings.ToLower(k)] = n
	}
	for _, k := range raw.NeverAuto {
		c.NeverAuto = append(c.NeverAuto, strings.ToLower(strings.TrimSpace(k)))
	}
	for id, p := range raw.Inboxes {
		if c.Inboxes == nil {
			c.Inboxes = map[string]InboxPolicy{}
		}
		q := InboxPolicy{FirstReply: strings.ToLower(strings.TrimSpace(p.FirstReply))}
		if len(p.Thresholds) > 0 {
			q.Thresholds = map[string]int{}
		}
		for k, n := range p.Thresholds {
			q.Thresholds[strings.ToLower(k)] = n
		}
		c.Inboxes[strings.ToLower(strings.TrimSpace(id))] = q
	}
	c.Supervised.Enabled, c.Supervised.Started = raw.Supervised.Enabled, raw.Supervised.Started
	if raw.Supervised.Days != 0 {
		c.Supervised.Days = raw.Supervised.Days
	}
	return c, c.Validate()
}

// Validate checks the configuration.
func (c Config) Validate() error {
	var bad []string
	for k, n := range c.Thresholds {
		if n < 0 {
			bad = append(bad, fmt.Sprintf("thresholds.%s: %d is negative", k, n))
		}
	}
	for k := range c.Thresholds {
		if strings.EqualFold(k, UnsubscribeClass) {
			bad = append(bad, fmt.Sprintf("thresholds.%s: unsubscribe is irreversible and always held; it takes no threshold", k))
		}
	}
	for _, s := range c.Seed {
		if !contains(Seedable, strings.ToLower(s)) {
			bad = append(bad, fmt.Sprintf("seed: %q cannot be seeded; only %s", s, strings.Join(Seedable, ", ")))
		}
	}
	bad = append(bad, c.validateNeverAuto()...)
	for id, p := range c.Inboxes {
		if strings.TrimSpace(id) == "" {
			bad = append(bad, "inboxes: an empty inbox ID")
		}
		switch p.FirstReply {
		case "", FirstReplyHold, FirstReplyAllow:
		default:
			bad = append(bad, fmt.Sprintf("inboxes.%s.first_reply: %q, want %s or %s", id, p.FirstReply, FirstReplyHold, FirstReplyAllow))
		}
		for k, n := range p.Thresholds {
			switch {
			case n < 0:
				bad = append(bad, fmt.Sprintf("inboxes.%s.thresholds.%s: %d is negative", id, k, n))
			case strings.EqualFold(k, UnsubscribeClass):
				bad = append(bad, fmt.Sprintf("inboxes.%s.thresholds.%s: unsubscribe is irreversible and always held; it takes no threshold", id, k))
			}
		}
	}
	if c.Supervised.Days <= 0 {
		bad = append(bad, fmt.Sprintf("supervised.days: %d, want at least 1", c.Supervised.Days))
	}
	for k, s := range c.Supervised.Started {
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			bad = append(bad, fmt.Sprintf("supervised.started.%s: %q is not an RFC 3339 time", k, s))
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		return fmt.Errorf("%s: %s", Key, strings.Join(bad, "; "))
	}
	return nil
}

// validateNeverAuto refuses the ceilings that mean nothing or contradict
// the rest of the configuration.
func (c Config) validateNeverAuto() []string {
	var bad []string
	for _, k := range c.NeverAuto {
		k = strings.ToLower(strings.TrimSpace(k))
		switch {
		case k == "":
			bad = append(bad, "never_auto: an empty class")
		case k == UnsubscribeClass:
			bad = append(bad, "never_auto: unsubscribe is already always held; name another class")
		case c.Threshold(k) <= 0:
			bad = append(bad, fmt.Sprintf("never_auto: %s has threshold 0, always automatic; a ceiling needs a threshold of at least 1", k))
		}
	}
	for _, s := range c.Seed {
		if c.neverAuto(strings.ToLower(s)) {
			bad = append(bad, fmt.Sprintf("never_auto: %q is also seeded; a never-automatic class cannot be seeded", s))
		}
	}
	return bad
}

// neverAuto reports whether class sits under the never_auto ceiling:
// named exactly, or through its base before ':'.
func (c Config) neverAuto(class string) bool {
	class = strings.ToLower(class)
	if contains(c.NeverAuto, class) {
		return true
	}
	base, _, ok := strings.Cut(class, ":")
	return ok && contains(c.NeverAuto, base)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Class names the threshold class of a write: its verb and the target
// class heo gives the hold point.
func Class(verb, class string) string {
	verb, class = strings.ToLower(verb), strings.ToLower(class)
	switch verb {
	case "reply":
		return "reply:" + class
	case "tag":
		return "label"
	case "unlabel":
		verb = "label"
	case "move":
		return "move_to"
	case "state":
		switch class {
		case "read", "unread", "star", "unstar", "archive", "trash", "spam":
			return class
		}
	case "unsubscribe", "attention":
		return verb
	}
	if class == "" {
		return verb
	}
	return verb + ":" + class
}

// Threshold is the clean approvals a class needs, whatever the inbox:
// ThresholdFor with no inbox.
func (c Config) Threshold(class string) int { return c.ThresholdFor("", class) }

// ThresholdFor is the clean approvals a class needs on inbox: the
// inbox's own entry for the exact class, else for its base before ':';
// then the ladder's, exact class then base; else DefaultThreshold.
func (c Config) ThresholdFor(inbox, class string) int {
	class = strings.ToLower(class)
	base, _, hasBase := strings.Cut(class, ":")
	lookups := []map[string]int{c.Inboxes[strings.ToLower(inbox)].Thresholds, c.Thresholds}
	for _, m := range lookups {
		if n, ok := m[class]; ok {
			return n
		}
		if hasBase {
			if n, ok := m[base]; ok {
				return n
			}
		}
	}
	return DefaultThreshold
}

// firstReply is the policy of inbox for a first reply to an unknown
// contact: hold unless the inbox says allow.
func (c Config) firstReply(inbox string) string {
	if c.Inboxes[strings.ToLower(inbox)].FirstReply == FirstReplyAllow {
		return FirstReplyAllow
	}
	return FirstReplyHold
}

// Facts are what a write says about itself that always holds it.
type Facts struct {
	// AddedCC: a reply with recipients added beyond the original's.
	AddedCC bool
	// ReplyToDiffers: the original's Reply-To is not its From.
	ReplyToDiffers bool
	// CarriedOver: an operation carried over from an earlier batch.
	CarriedOver bool
	// FirstReplyUnknown: a reply to a contact heo never wrote to on
	// this inbox. heo computes it from its own records; no caller
	// states it.
	FirstReplyUnknown bool
}

// Counts are uma's counters of one trust key. Found is false when uma
// has none.
type Counts struct {
	Found       bool
	Clean       int64
	Corrections int64
}

// Input is one write to decide.
type Input struct {
	OnBehalfOf, Channel, Verb, Class string
	Facts
	Counts Counts
	Now    time.Time
}

// Decision is the ladder's answer.
type Decision struct {
	Hold   bool
	Reason string
}

func held(r string) Decision { return Decision{Hold: true, Reason: r} }

// Decide applies the ladder: the always-held rules first (including an
// unsubscribe, which no threshold or seed reaches), then the
// supervised week, then the seed and the earned count.
func (c Config) Decide(in Input) Decision {
	class := Class(in.Verb, in.Class)
	if r := c.guard(in, class); r != "" {
		return held(r)
	}
	if c.Supervised.Enabled && c.supervised(in) {
		return held("supervised first week")
	}
	need := c.ThresholdFor(in.Channel, class)
	if need <= 0 {
		return Decision{}
	}
	if contains(c.Seed, class) && in.Counts.Corrections == 0 {
		return Decision{}
	}
	if in.Counts.Clean >= int64(need) {
		return Decision{}
	}
	return held(fmt.Sprintf("training: %s %d of %d clean approvals", class, in.Counts.Clean, need))
}

// guard is the reason a write is held whatever its count: the
// always-held rules and the never_auto ceiling. "" when none applies.
func (c Config) guard(in Input, class string) string {
	for _, ch := range c.ApprovalAlways {
		if ch == in.Channel {
			return "mailbox marked approval-always"
		}
	}
	switch {
	case in.AddedCC:
		return "reply adds recipients (cc)"
	case in.ReplyToDiffers:
		return "Reply-To differs from From"
	case in.FirstReplyUnknown && class == "reply:send" && c.firstReply(in.Channel) == FirstReplyHold:
		return FirstReplyReason
	case c.Unattended && class == "reply:send":
		return "unattended send"
	case in.CarriedOver:
		return "carried over from an earlier batch"
	case class == UnsubscribeClass:
		return UnsubscribeReason
	case c.neverAuto(class):
		return fmt.Sprintf("%s %s is capped at asking in batches (held_operations.trust.never_auto)", NeverAutoReasonPrefix, class)
	}
	return ""
}

// Level is where a trust key stands on the ladder, as a desk shows it.
type Level string

// The levels.
const (
	// LevelAskEach: no clean approval yet (count 0); every write is held.
	LevelAskEach Level = "ask_each"
	// LevelAskInBatches: at least one clean approval, short of the
	// threshold; writes are held and a batch approval decides them.
	LevelAskInBatches Level = "ask_in_batches"
	// LevelAuto: at or above the threshold (or seeded, or threshold 0).
	LevelAuto Level = "auto"
	// LevelNeverAuto: held whatever the count: the guards (unsubscribe,
	// approval-always mailbox, unattended send) and never_auto classes.
	LevelNeverAuto Level = "never_auto"
)

// Label is the level's name as a desk shows it.
func (l Level) Label() string {
	switch l {
	case LevelAskEach:
		return "Ask each"
	case LevelAskInBatches:
		return "Ask in batches"
	case LevelAuto:
		return "Auto"
	case LevelNeverAuto:
		return "Never auto"
	}
	return string(l)
}

// Level derives the level of the key in from the configuration. The
// supervised week holds without changing it: it is a stretch of time,
// not a place on the ladder.
func (c Config) Level(in Input) Level {
	class := Class(in.Verb, in.Class)
	if c.guard(in, class) != "" {
		return LevelNeverAuto
	}
	need := c.ThresholdFor(in.Channel, class)
	switch {
	case need <= 0, contains(c.Seed, class) && in.Counts.Corrections == 0, in.Counts.Clean >= int64(need):
		return LevelAuto
	case in.Counts.Clean <= 0:
		return LevelAskEach
	}
	return LevelAskInBatches
}

// IsNeverAuto reports whether the class of a write (verb and target class)
// sits under the never_auto ceiling.
func (c Config) IsNeverAuto(verb, class string) bool { return c.neverAuto(Class(verb, class)) }

// ThresholdOf is the clean approvals the class of a write on inbox needs: nil
// for a class that has none (unsubscribe is never automatic).
func (c Config) ThresholdOf(inbox, verb, class string) *int {
	cl := Class(verb, class)
	if cl == UnsubscribeClass {
		return nil
	}
	n := c.ThresholdFor(inbox, cl)
	return &n
}

func (c Config) supervised(in Input) bool {
	for k, s := range c.Supervised.Started {
		if strings.EqualFold(k, in.OnBehalfOf+"/"+in.Channel) {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return true
			}
			return in.Now.Before(t.AddDate(0, 0, c.Supervised.Days))
		}
	}
	return true
}
