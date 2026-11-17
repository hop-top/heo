//go:build heoe2e

package authz

import "hop.top/uma/model"

// The e2e build runs uma's a2a.messages adapter so a remote agent can
// open and cancel tasks over JSON-RPC. The adapter reads and acts as
// the runtime's own actor: its task sink asks task.read,
// conversation.read and message.read to answer a protocol read, and
// task.act to record a cancel request. The release policy grants the
// runtime none of them, so a release build cannot serve that adapter;
// this build grants them, for the e2e tests alone.
func init() {
	serviceActions = append(serviceActions,
		model.ActionTaskRead, model.ActionConversationRead, model.ActionMessageRead, model.ActionTaskAct)
}
