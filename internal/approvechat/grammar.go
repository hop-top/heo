package approvechat

import "strings"

// Reactions that decide. Anything else is ignored.
var (
	approveEmoji = map[string]bool{"👍": true, "✅": true}
	rejectEmoji  = map[string]bool{"👎": true, "❌": true}
)

// Command is what a reply or reaction asks.
type Command struct {
	Decision string // "approve" or "reject"
	// Operation is the operation ID the text names, empty when it
	// relies on the announcement it answers.
	Operation string
	Note      string
}

// ParseReaction reads a reaction: 👍 or ✅ approves, 👎 or ❌ rejects. The
// operation is the announcement the reaction is on.
func ParseReaction(emoji string) (Command, bool) {
	// A skin tone or variation selector makes a distinct emoji; trim
	// the selector only.
	e := strings.TrimSuffix(emoji, "️")
	switch {
	case approveEmoji[e]:
		return Command{Decision: "approve"}, true
	case rejectEmoji[e]:
		return Command{Decision: "reject"}, true
	}
	return Command{}, false
}

// ParseReply reads a reply: `approve [note]` or `reject [note]` as a
// reply to an announcement, or `approve #<operation> [note]` anywhere in
// the conversation. The verb is the first word, case-insensitive; any
// other text is conversation and is ignored. Outside a reply a decision
// must name its operation.
func ParseReply(text string, inReply bool) (Command, bool) {
	f := strings.Fields(text)
	if len(f) == 0 {
		return Command{}, false
	}
	var c Command
	switch strings.ToLower(strings.TrimRight(f[0], ".!:,")) {
	case "approve":
		c.Decision = "approve"
	case "reject":
		c.Decision = "reject"
	default:
		return Command{}, false
	}
	rest := f[1:]
	if len(rest) > 0 && strings.HasPrefix(rest[0], "#") && len(rest[0]) > 1 {
		c.Operation, rest = strings.TrimPrefix(rest[0], "#"), rest[1:]
	}
	if c.Operation == "" && !inReply {
		return Command{}, false
	}
	c.Note = strings.Join(rest, " ")
	return c, true
}
