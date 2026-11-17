package hooks

import (
	"hop.top/heo/internal/bridge"
	"hop.top/uma/model"
)

// conversationOf is the conversation an event names, "" when it names
// none.
func conversationOf(ev model.Event) string {
	_, p := bridge.Map(ev)
	return p.Conversation
}
