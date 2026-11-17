package daemon

import (
	"strings"
	"testing"

	"hop.top/heo/internal/approvechat"
	"hop.top/heo/internal/authz"
)

// TestRefuseSelf: the approval chat's own actor, which wrote the
// announcement, is never a member that decides.
func TestRefuseSelf(t *testing.T) {
	cfg := approvechat.Config{Enabled: true, Conversations: []approvechat.Conversation{{
		Inbox: "in", Conversation: "cv",
		Members: []approvechat.Member{{Identity: "idn-a", Actor: "uid:1"}, {Identity: "idn-b", Actor: authz.ApprovalChatActor}},
	}}}
	if err := refuseSelf(cfg); err == nil || !strings.Contains(err.Error(), "own actor") {
		t.Fatalf("refuseSelf = %v", err)
	}
	cfg.Conversations[0].Members = cfg.Conversations[0].Members[:1]
	if err := refuseSelf(cfg); err != nil {
		t.Fatalf("refuseSelf = %v", err)
	}
}
