package cmd

import (
	"testing"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/settings"
	"hop.top/uma/model"
)

// TestHoldGuard: with holding on, a plugin actor's write that has no
// hold path is refused as UNSUPPORTED; reads (an extension query, the
// provider mailbox list, included), hold-aware verbs, people
// and the off switch pass.
func TestHoldGuard(t *testing.T) {
	plugin := control.Caller{Actor: "plugin:triage"}
	person := control.Caller{Actor: "uid:1001"}
	a := newApp()
	if err := a.holdGuard(plugin, model.ActionWorkItemAct); err != nil {
		t.Fatalf("holding off: %v", err)
	}
	a.r.Viper.Set(settings.KeyHeldEnabled, true)
	for _, act := range []string{model.ActionMessageRead, model.ActionConversationRead, model.ActionEventList, model.ActionExtensionQuery,
		model.ActionMessageReply, model.ActionMessageEdit, model.ActionConversationTransition, model.ActionConversationLabel,
		model.ActionLabelConfigure, model.ActionMessageNote, model.ActionConversationStart} {
		if err := a.holdGuard(plugin, act); err != nil {
			t.Errorf("%s refused: %v", act, err)
		}
	}
	for _, act := range []string{model.ActionWorkItemAct, model.ActionContactWrite, model.ActionConversationMarkRead,
		model.ActionConversationAssign, model.ActionTaskAct, model.ActionExtensionExecute} {
		if !model.IsCode(a.holdGuard(plugin, act), model.Unsupported) {
			t.Errorf("%s not refused for a plugin", act)
		}
		if err := a.holdGuard(person, act); err != nil {
			t.Errorf("%s refused for a person: %v", act, err)
		}
	}
	if h := a.holds(); h == nil || h.Expiry != settings.DefaultHeldExpiry && h.Expiry <= 0 {
		t.Fatalf("holds = %+v", h)
	}
}

// TestHeldPolicy: uma holds a label create and a conversation transition
// only when opted in, which the daemon does while holding is on.
func TestHeldPolicy(t *testing.T) {
	a := newApp()
	if p := heldPolicy(a.r.Viper); p.LabelConfigure || p.ConversationTransition {
		t.Fatalf("holding off opted in: %+v", p)
	}
	a.r.Viper.Set(settings.KeyHeldEnabled, true)
	if p := heldPolicy(a.r.Viper); !p.LabelConfigure || !p.ConversationTransition || p.StartCreateAtHold {
		t.Fatalf("holding on: %+v", p)
	}
}
