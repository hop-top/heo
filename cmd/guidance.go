package cmd

import (
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
)

// Kit's guidance tier reads two annotations on every runnable leaf:
// kit/examples (literal invocations) and, on a leaf that is not a
// read, kit/next-steps (the commands that usually follow). They are
// what `heo spec` and the served surfaces hand an agent.
//
// Examples have one source of truth, the cobra Example text a command
// already carries: applyGuidance derives kit/examples from it. A leaf
// that has no such text (kit's own verbs, and the two `config path`
// leaves) lists its examples in leafExamples, and applyGuidance renders
// those back into Example, so `--help` shows them too. Next steps live
// in leafNextSteps. guidance_test.go parses every one of them against
// the command tree: a verb, flag or argument count that does not exist
// fails there.

// The provider state verbs share their next steps: the real run after a
// preview, and the operation that follows the change to the provider.
func init() {
	for _, v := range desk.StateVerbs {
		path, suggest := "heo conversation "+string(v), "heo conversation "+string(v)+" <conversation>"
		if v == "move" {
			suggest += " --role <role>"
		}
		leafNextSteps[path] = []cli.NextStep{
			{When: "after a --dry-run preview", Suggest: suggest, Reason: "the real change; the preview showed the scope and limits the channel reports"},
			{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the change is queued, not applied: follow it to the provider"},
			{When: "after the provider applied it", Suggest: "heo conversation get <conversation>", Reason: "read the provider state uma recorded for the conversation"},
		}
	}
}

// leafExamples holds the examples of the leaves that carry no cobra
// Example text, keyed by command path.
var leafExamples = map[string][]cli.Example{
	"heo config path": {
		{Title: "Where the profile's configuration file lives", Command: "heo config path"},
		{Title: "For another profile", Command: "heo --profile work config path"},
	},
	"heo config paths": {
		{Title: "Every layer the configuration is read from", Command: "heo config paths"},
		{Title: "JSON for agents", Command: "heo config paths --format json"},
	},
	"heo serve": {
		{Title: "Run the control socket in the foreground (deprecated spelling of service start)", Command: "heo serve socket"},
	},
	"heo service list": {
		{Title: "Which services exist, are enabled and are ready", Command: "heo service list"},
		{Title: "JSON for agents", Command: "heo service list --format json"},
	},
	"heo service restart": {
		{Title: "Drain and re-execute the running daemon", Command: "heo service restart"},
		{Title: "Give the new instance longer to become ready", Command: "heo service restart --wait 5m"},
		{Title: "See the plan, change nothing", Command: "heo service restart --dry-run"},
	},
	"heo service start": {
		{Title: "Run every enabled service in the foreground", Command: "heo service start"},
		{Title: "Run one service, enabled or not", Command: "heo service start socket"},
		{Title: "Enable another service for this run", Command: "heo service start --enable webhook"},
	},
	"heo service status": {
		{Title: "Which services the running daemon started and whether they are ready", Command: "heo service status"},
		{Title: "JSON for agents", Command: "heo service status --format json"},
	},
	"heo service stop": {
		{Title: "Drain the running daemon", Command: "heo service stop"},
		{Title: "Give the drain longer", Command: "heo service stop --wait 3m"},
		{Title: "See the plan, change nothing", Command: "heo service stop --dry-run"},
	},
	"heo token claims": {
		{Title: "A claims template for an external signer", Command: "heo token claims --sub ada --scopes read,write --expires 1h"},
	},
	"heo token decode": {
		{Title: "Inspect a token's payload without verifying it", Command: "heo token decode <token>"},
	},
	"heo token verify": {
		{Title: "Verify a token against the api service's authentication", Command: "heo token verify <token>"},
		{Title: "Against another service", Command: "heo token verify <token> --service mcp"},
	},
}

// leafNextSteps holds, per leaf, the commands that usually follow it,
// keyed by command path. Kit requires them on every leaf that is not a
// read; `status` is a read whose kit default points at commands heo
// does not have, so heo replaces it. A step names the real next
// invocation (<angle brackets> mark what the caller fills in) and why.
var leafNextSteps = map[string][]cli.NextStep{
	"heo status": {
		{When: "the daemon's state is needed", Suggest: "heo service status", Reason: "status reports the tool and the profile; the running daemon's services are service status"},
		{When: "a setting looks wrong", Suggest: "heo config list", Reason: "every effective value, with the layer it comes from"},
	},
	"heo channel create": {
		{When: "on success", Suggest: "heo channel check <channel> --remote", Reason: "ask the provider whether the credential and configuration work"},
		{When: "channel works", Suggest: "heo inbox create --channel <channel> --key <key> --destination <destination> --mode per_contact", Reason: "an inbox is what conversations and sends go through"},
	},
	"heo channel update": {
		{When: "on success", Suggest: "heo channel check <channel> --remote", Reason: "ask the provider whether the new credential or configuration works"},
		{When: "on success", Suggest: "heo channel get <channel>", Reason: "read back the stored channel and its new revision"},
	},
	"heo config set": {
		{When: "on success", Suggest: "heo config get <key>", Reason: "read the value back with the layer it comes from"},
		{When: "before starting the daemon", Suggest: "heo config validate", Reason: "check the whole configuration, not only this key"},
	},
	"heo contact create": {
		{When: "on success", Suggest: "heo contact get <contact>", Reason: "read the contact back with its revision, which updates and merges need"},
	},
	"heo contact update": {
		{When: "on success", Suggest: "heo contact get <contact>", Reason: "read back the contact and its new revision"},
	},
	"heo contact merge": {
		{When: "after a --dry-run preview", Suggest: "heo contact merge <from> <into> --reason <reason> --revision <revision> --into-revision <revision>", Reason: "the real merge; on a terminal it asks to confirm, unattended it needs --confirm=yes"},
		{When: "after a merge", Suggest: "heo contact get <into>", Reason: "the surviving contact now holds the merged identities"},
		{When: "after a merge", Suggest: "heo identity list --contact <into>", Reason: "list the identities that moved to it"},
	},
	"heo channel authorize": {
		{When: "after a --dry-run preview", Suggest: "heo channel authorize <channel> --client-id <client-id>", Reason: "the real consent: it opens your browser and keeps the refresh token the provider answers with"},
		{When: "on success", Suggest: "heo channel check <channel> --remote", Reason: "mint an access token from the stored grant and see the grant kind and the token's expiry"},
	},
	"heo conversation label": {
		{When: "on success", Suggest: "heo conversation get <conversation>", Reason: "read back the conversation and its labels"},
		{When: "to find the labelled conversations", Suggest: "heo conversation list --label <label>", Reason: "filter the list by the label just applied"},
	},
	"heo conversation status": {
		{When: "after a --dry-run preview", Suggest: "heo conversation status <conversation> --set <status>", Reason: "the real change; the provider owns the status, so it is queued, not applied"},
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the change is queued, not applied: follow it to the provider"},
		{When: "after the provider reports it", Suggest: "heo conversation get <conversation>", Reason: "read the work item's status and the provider's own"},
		{When: "to browse the tracker", Suggest: "heo conversation list --work-item-status <status>", Reason: "list the conversations whose work item is in a status"},
	},
	"heo conversation update": {
		{When: "after a --dry-run preview", Suggest: "heo conversation update <conversation> --field <key=value>", Reason: "the real change; the provider's fields are queued, not applied"},
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the update is queued, not applied: follow it to the provider"},
		{When: "after the provider reports it", Suggest: "heo conversation get <conversation>", Reason: "read the work item's fields as the provider last reported them"},
	},
	"heo conversation tag": {
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the change is queued, not applied: follow it to the mail provider; uma records no provider tags"},
	},
	"heo conversation unsubscribe": {
		{When: "after a --dry-run preview", Suggest: "heo conversation unsubscribe <conversation>", Reason: "the real request; it is irreversible, and a plugin actor's is held for a decision first"},
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the request is queued, not sent: follow it, and see whether the adapter's rules allowed it"},
	},
	"heo provider-label create": {
		{When: "after a --dry-run preview", Suggest: "heo provider-label create <name> --channel <channel>", Reason: "the real create; the preview said whether the provider holds the name already"},
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the create is queued, not made: follow it, and read the new mailbox's ID among the provider references"},
		{When: "after it settled", Suggest: "heo provider-label list --channel <channel>", Reason: "read the mailbox back with its ID"},
	},
	"heo message reply-draft": {
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the draft is queued, not made: follow it to the mailbox, where its provider ID shows for a later update or delete"},
		{When: "to send the reply yourself", Suggest: "heo message reply <conversation> --text <text>", Reason: "a draft sends nothing; a reply does"},
	},
	"heo conversation assign": {
		{When: "after a --dry-run preview", Suggest: "heo conversation assign <conversation> --to <actor>", Reason: "the real assignment; the preview showed who holds the conversation now"},
		{When: "on success", Suggest: "heo conversation get <conversation>", Reason: "read back the assignee and the new revision"},
		{When: "to find your queue", Suggest: "heo conversation list --assignee self", Reason: "uma filters by assignee; snoozed conversations stay out unless --snoozed says otherwise"},
	},
	"heo conversation unassign": {
		{When: "on success", Suggest: "heo conversation get <conversation>", Reason: "read back the cleared assignee and the new revision"},
		{When: "to find what nobody holds", Suggest: "heo conversation list --assignee none", Reason: "uma filters the unassigned conversations"},
	},
	"heo conversation snooze": {
		{When: "after a --dry-run preview", Suggest: "heo conversation snooze <conversation> --until <time>", Reason: "the real snooze, with the resolved time the preview printed; --for resolves a new time on every run"},
		{When: "to see what is hidden", Suggest: "heo conversation list --snoozed only", Reason: "snoozed conversations are left out of the default list"},
		{When: "snoozed by mistake", Suggest: "heo conversation unsnooze <conversation>", Reason: "bring it back to the default list now"},
	},
	"heo conversation unsnooze": {
		{When: "on success", Suggest: "heo conversation get <conversation>", Reason: "read back the conversation without its snooze"},
		{When: "to answer the customer", Suggest: "heo message reply <conversation> --text <text>", Reason: "reply in the conversation that is back in the list"},
	},
	"heo conversation link": {
		{When: "after a --dry-run preview", Suggest: "heo conversation link <conversation> --conversation <other>", Reason: "the real link; the preview showed both ends"},
		{When: "on success", Suggest: "heo conversation get <conversation>", Reason: "read back the links the conversation holds"},
		{When: "to find the linked ones", Suggest: "heo conversation list --linked", Reason: "uma filters the conversations that hold a link you may read"},
	},
	"heo conversation unlink": {
		{When: "on success", Suggest: "heo conversation get <conversation>", Reason: "read back the links the conversation still holds"},
	},
	"heo conversation mark-read": {
		{When: "on success", Suggest: "heo conversation list --unread", Reason: "what is still unread for you; read state is yours alone"},
		{When: "to tell the provider's sender it was read", Suggest: "heo receipt send --message <message> --kind read", Reason: "marking read is local and sends no receipt; this does, where the channel supports it"},
	},
	"heo conversation mark-unread": {
		{When: "on success", Suggest: "heo conversation list --unread", Reason: "the conversation is unread for you again; no one else's marker moved"},
	},
	"heo conversation summarize": {
		{When: "to see what the call would send before it does", Suggest: "heo conversation summarize <conversation> --dry-run", Reason: "the provider, its host class (local or cloud), the message count and the prompt size; nothing is sent"},
		{When: "to check the summary against the source", Suggest: "heo message list --conversation <conversation>", Reason: "the messages themselves; the summary is machine-generated"},
	},
	"heo conversation triage": {
		{When: "to accept a suggested label", Suggest: "heo conversation label <conversation> --add <label>", Reason: "triage applies nothing; this applies the label you accepted"},
		{When: "to see the labels triage may suggest from", Suggest: "heo label list", Reason: "the tenant's own labels; a name outside them is dropped"},
	},
	"heo conversation reopen": {
		{When: "on success", Suggest: "heo conversation get <conversation>", Reason: "read back the status and the new revision"},
		{When: "to answer the customer", Suggest: "heo message reply <conversation> --text <text>", Reason: "reply in the reopened conversation"},
	},
	"heo conversation resolve": {
		{When: "on success", Suggest: "heo conversation get <conversation>", Reason: "read back the status and the new revision"},
		{When: "resolved by mistake", Suggest: "heo conversation reopen <conversation> --revision <revision>", Reason: "put it back in the open queue"},
	},
	"heo operation held approve": {
		{When: "on success", Suggest: "heo operation watch <operation>", Reason: "follow the approved operation until a worker applies it"},
		{When: "refused as stale", Suggest: "heo operation held show <operation>", Reason: "read the proposal again before deciding; --force decides anyway"},
		{When: "to approve a class in bulk", Suggest: "heo operation held approve --class <class> --inbox <inbox> --dry-run", Reason: "list what the selector matches; the real run approves them under one batch, counted once per trust key"},
	},
	"heo operation held edit": {
		{When: "after a --dry-run preview", Suggest: "heo operation held edit <operation> --args <file>", Reason: "the real edit: queues the operation with your arguments and counts a correction"},
		{When: "refused as stale", Suggest: "heo operation held show <operation> --format json", Reason: "read the proposal again and edit what you now see; an edit has no --force"},
		{When: "on success", Suggest: "heo operation watch <operation>", Reason: "follow the edited operation until a worker applies it"},
	},
	"heo operation trust import": {
		{When: "before importing", Suggest: "heo operation trust import --file <file> --dry-run", Reason: "see each key as the import would leave it and which would become automatic; writes nothing"},
		{When: "on success", Suggest: "heo operation trust list --inbox <inbox>", Reason: "the imported counters and which writes are now automatic"},
	},
	"heo operation trust set": {
		{When: "before changing a count", Suggest: "heo operation trust set <key> --inbox <inbox> --reset --dry-run", Reason: "the count and level it would leave; writes nothing"},
		{When: "on success", Suggest: "heo operation trust list --inbox <inbox>", Reason: "the key's count, threshold and level now"},
	},
	"heo operation held reject": {
		{When: "on success", Suggest: "heo operation held list", Reason: "the proposals still waiting for a decision"},
	},
	"heo hook dead-letter retry": {
		{When: "after a --dry-run preview", Suggest: "heo hook dead-letter retry <plugin> <event>", Reason: "the real retry: queues a job that runs the plugin and applies its operations under the keys of the first run"},
		{When: "on success", Suggest: "heo hook job get <job>", Reason: "the job is queued, not run: its state and outcome"},
		{When: "failed again", Suggest: "heo hook test <plugin> --event <event>", Reason: "run the plugin on the event and read its answer without applying anything"},
	},
	"heo hook replay": {
		{When: "after a --dry-run preview", Suggest: "heo hook replay <plugin> --from <position>", Reason: "the real replay: queues a job for the events the preview named"},
		{When: "on success", Suggest: "heo hook job get <job>", Reason: "the job is queued, not run: its counts and next position"},
		{When: "events failed", Suggest: "heo hook dead-letter list --plugin <plugin>", Reason: "a failed event is recorded as a dead letter, to retry or drop"},
	},
	"heo hook dead-letter drop": {
		{When: "after a --dry-run preview", Suggest: "heo hook dead-letter drop <plugin> <event> --confirm=yes", Reason: "the real drop; on a terminal it asks to confirm, unattended it needs --confirm=yes"},
		{When: "on success", Suggest: "heo hook dead-letter list --plugin <plugin> --state all", Reason: "the record stays listed as discarded"},
	},
	"heo delivery reconcile": {
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "read the delivery's state after reconciling with the provider"},
		{When: "reported dead_lettered, not_needed", Suggest: "heo delivery retry <operation> --dry-run", Reason: "retry takes dead_lettered deliveries only; the dry run checks and shows the replay"},
	},
	"heo delivery retry": {
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "read the delivery's state"},
		{When: "to wait for the outcome", Suggest: "heo operation watch <operation>", Reason: "follow the operation until it settles"},
	},
	"heo event watch": {
		{When: "to look at one event", Suggest: "heo event get <event>", Reason: "read the full event behind an ID the watch printed"},
		{When: "after the watch ends", Suggest: "heo event list --cursor <cursor>", Reason: "page on from the checkpoint the watch printed; a list moves no checkpoint"},
	},
	"heo identity link": {
		{When: "on success", Suggest: "heo identity get <identity>", Reason: "read back the identity and the contact it now points at"},
		{When: "on success", Suggest: "heo contact get <contact>", Reason: "read the contact the identity was linked to"},
	},
	"heo identity unlink": {
		{When: "on success", Suggest: "heo identity get <identity>", Reason: "read back the identity, which no longer points at the contact"},
	},
	"heo inbox create": {
		{When: "on success", Suggest: "heo inbox get <inbox>", Reason: "read back the inbox with its revision"},
		{When: "inbox exists", Suggest: "heo message send --inbox <inbox> --to <recipient> --text <text>", Reason: "start an exchange through it"},
	},
	"heo inbox disable": {
		{When: "on success", Suggest: "heo inbox get <inbox>", Reason: "read back the inbox and its new revision"},
		{When: "to undo", Suggest: "heo inbox enable <inbox> --revision <revision>", Reason: "turn it back on"},
	},
	"heo inbox enable": {
		{When: "on success", Suggest: "heo inbox get <inbox>", Reason: "read back the inbox and its new revision"},
	},
	"heo inbox update": {
		{When: "on success", Suggest: "heo inbox get <inbox>", Reason: "read back the inbox and its new revision"},
	},
	"heo label create": {
		{When: "on success", Suggest: "heo label list", Reason: "see the label among the others"},
		{When: "to use it", Suggest: "heo conversation label <conversation> --add <label>", Reason: "put the label on a conversation"},
	},
	"heo message delete": {
		{When: "after a --dry-run preview", Suggest: "heo message delete <message> --revision <revision>", Reason: "the real delete; on a terminal it asks to confirm, unattended it needs --confirm=yes"},
		{When: "after a delete", Suggest: "heo delivery get <operation>", Reason: "the delete is queued: the message is a tombstone only once the provider applied it"},
		{When: "after the delivery settles", Suggest: "heo message get <message>", Reason: "read back the tombstone"},
	},
	"heo message draft": {
		{When: "to preview sending the draft", Suggest: "heo message reply <conversation> --text \"<draft>\" --dry-run", Reason: "the exact destination and visibility; the draft itself is never sent by this command"},
		{When: "to pipe the draft into the preview", Suggest: "heo message draft <conversation> --format text", Reason: "prints the draft alone on stdout, status lines on stderr"},
	},
	"heo message edit": {
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "follow the edit's delivery to the provider"},
		{When: "on success", Suggest: "heo message get <message>", Reason: "read back the edited message"},
	},
	"heo message reply": {
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the reply is queued, not delivered: follow its delivery"},
		{When: "to wait for the outcome", Suggest: "heo operation watch <operation>", Reason: "follow the operation until it settles"},
	},
	"heo message send": {
		{When: "after a --dry-run preview", Suggest: "heo message send --inbox <inbox> --to <recipient> --text <text>", Reason: "the real send, once the preview names the right destination"},
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the message is queued, not delivered: follow its delivery"},
		{When: "to wait for the outcome", Suggest: "heo operation watch <operation>", Reason: "follow the operation until it settles"},
	},
	"heo note add": {
		{When: "on success", Suggest: "heo note list --conversation <conversation>", Reason: "see the note among the conversation's notes"},
	},
	"heo history fetch": {
		{When: "after a --dry-run preview", Suggest: "heo history fetch --conversation <conversation>", Reason: "the real import, once the preview names the right scope and the adapter that declares history"},
		{When: "the report names a cursor", Suggest: "heo history fetch --conversation <conversation> --cursor <cursor>", Reason: "the window was longer than the limit: resume where it stopped"},
		{When: "on success", Suggest: "heo message list --conversation <conversation>", Reason: "read the imported messages among the conversation's own"},
	},
	"heo interaction open-modal": {
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the modal is queued, not shown: follow it to Slack, which must accept the trigger_id inside its window"},
		{When: "once the operation settled", Suggest: "heo operation get <operation>", Reason: "its provider references carry the opened view's ID, for update-modal"},
	},
	"heo interaction push-modal": {
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the view is queued, not shown: follow it to Slack, which must accept the trigger_id inside its window"},
		{When: "once the operation settled", Suggest: "heo operation get <operation>", Reason: "its provider references carry the pushed view's ID, for update-modal"},
	},
	"heo interaction update-modal": {
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the update is queued, not applied: follow it to Slack"},
	},
	"heo interaction respond": {
		{When: "after a --dry-run preview", Suggest: "heo interaction respond <interaction> --mode <mode> --text <text>", Reason: "the real answer; the preview showed the audience, the window and the adapter that declares respond"},
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the response is queued, not given: follow it to the provider, which shows it only once it accepts it"},
		{When: "after the delivery settles", Suggest: "heo interaction get <interaction>", Reason: "read back that the interaction is answered, and how many follow-ups it holds"},
	},
	"heo participant add": {
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the change is queued: follow it to the provider"},
		{When: "after the delivery settles", Suggest: "heo participant list --conversation <conversation>", Reason: "the participant is listed only once the provider applied it"},
	},
	"heo participant remove": {
		{When: "after a --dry-run preview", Suggest: "heo participant remove --conversation <conversation> --revision <revision> <who>", Reason: "the real removal; on a terminal it asks to confirm, unattended it needs --confirm=yes"},
		{When: "after a removal", Suggest: "heo delivery get <operation>", Reason: "the removal is queued: the participant leaves the list only once the provider applied it"},
		{When: "after the delivery settles", Suggest: "heo participant list --conversation <conversation>", Reason: "read back who still takes part"},
	},
	"heo reaction add": {
		{When: "on success", Suggest: "heo reaction list --message <message>", Reason: "see the reaction among the message's reactions"},
	},
	"heo reaction remove": {
		{When: "on success", Suggest: "heo reaction list --message <message>", Reason: "confirm the reaction is gone"},
	},
	"heo receipt send": {
		{When: "on success", Suggest: "heo delivery get <operation>", Reason: "the receipt is queued, not sent: follow it to the provider; whether the sender is shown it is the provider's"},
	},
	"heo serve": {
		{When: "daemon is up", Suggest: "heo service status", Reason: "see which services started and whether each is ready"},
	},
	"heo service restart": {
		{When: "on success", Suggest: "heo service status", Reason: "see the new instance's services and whether each is ready"},
	},
	"heo plugin token mint": {
		{When: "to give the plugin a role", Suggest: "heo config get authorization.grants", Reason: "read the grants, then add plugin:<name> with the roles it needs; it holds none until then"},
		{When: "to check the grant", Suggest: "heo config validate", Reason: "check the whole policy, the plugin's grant included"},
	},
	"heo plugin token revoke": {
		{When: "to remove the plugin's grant too", Suggest: "heo config get authorization.grants", Reason: "read the grants, then drop the plugin:<name> entry"},
	},
	"heo service setup": {
		{When: "on success", Suggest: "heo service restart", Reason: "a running daemon reads the new configuration only when it restarts"},
		{When: "no daemon running", Suggest: "heo service start", Reason: "start the daemon with the surfaces just enabled"},
		{When: "to verify", Suggest: "heo daemon ping", Reason: "ask the daemon who you are to it and how its runtime stands"},
	},
	"heo service start": {
		{When: "daemon is up", Suggest: "heo service status", Reason: "see which services started and whether each is ready"},
		{When: "daemon is up", Suggest: "heo daemon ping", Reason: "ask the daemon who you are to it and how its runtime stands"},
	},
	"heo service stop": {
		{When: "on success", Suggest: "heo service status", Reason: "confirm no instance is left running (exits 3 once it is gone)"},
	},
	"heo storage backup": {
		{When: "on success", Suggest: "heo storage restore <path> --dry-run", Reason: "check the backup restores, without touching the store"},
	},
	"heo storage migrate": {
		{When: "after a --dry-run preview", Suggest: "heo storage migrate", Reason: "apply the pending migrations"},
		{When: "on success", Suggest: "heo storage status", Reason: "read the schema version the store is now at"},
	},
	"heo storage restore": {
		{When: "after a --dry-run preview", Suggest: "heo storage restore <path>", Reason: "the real restore; on a terminal it asks to confirm, unattended it needs --confirm=yes"},
		{When: "after a restore", Suggest: "heo storage status", Reason: "the restored schema is the backup's: an older one reads migration_required, which storage migrate brings current"},
	},
	"heo task cancel": {
		{When: "on success", Suggest: "heo task get <task>", Reason: "read the task's state after the cancel"},
	},
	"heo task watch": {
		{When: "after the watch ends", Suggest: "heo task get <task>", Reason: "read the task's final state"},
	},
	"heo upgrade": {
		{When: "after a --dry-run preview", Suggest: "heo upgrade", Reason: "install the release the preview named"},
		{When: "on success", Suggest: "heo storage status", Reason: "an upgrade leaves the store alone: a newer schema reads migration_required, which storage migrate brings current"},
		{When: "a daemon was stopped for the upgrade", Suggest: "heo service start", Reason: "the upgrade is refused while a daemon runs, so start it again on the new binary"},
	},
}

// applyGuidance installs kit/examples on every runnable leaf of the
// tree and kit/next-steps on the ones the tables name. A leaf that
// already set its own examples (daemon ping, service setup) keeps
// them. Setters fail only on an oversized payload, which is a defect
// of the tables above, found by any test that builds the tree.
func applyGuidance(root *cobra.Command) {
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c.HasSubCommands() || !c.Runnable() {
			return
		}
		path := c.CommandPath()
		if _, ok := cli.GetExamples(c); !ok {
			ex := examplesFromText(c.Example)
			if len(ex) == 0 {
				ex = leafExamples[path]
			}
			if len(ex) > 0 {
				must(cli.SetExamples(c, ex))
			}
		}
		if c.Example == "" {
			c.Example = exampleText(c)
		}
		if ns := leafNextSteps[path]; len(ns) > 0 {
			must(cli.SetNextSteps(c, ns))
		}
	}
	walk(root)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// examplesFromText reads the commands out of a cobra Example block: one
// invocation per non-blank line, each starting with the binary name.
func examplesFromText(text string) []cli.Example {
	var out []cli.Example
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "heo ") {
			continue
		}
		out = append(out, cli.Example{Title: exampleTitle(line), Command: line})
	}
	return out
}

// exampleTitle labels an invocation by what sets it apart: a preview,
// a confirmed run, a retry-safe key, JSON output, else the flags it
// uses (or "Basic use" when it uses none).
func exampleTitle(line string) string {
	var flags []string
	for _, f := range strings.Fields(line) {
		if name, _, _ := strings.Cut(f, "="); strings.HasPrefix(name, "--") {
			flags = append(flags, name)
		}
	}
	has := func(flag string) bool {
		for _, f := range flags {
			if f == flag {
				return true
			}
		}
		return false
	}
	switch {
	case has("--dry-run"):
		return "Preview with --dry-run, change nothing"
	case has("--confirm"):
		return "Unattended, with --confirm=yes"
	case has("--idempotency-key"):
		return "Safe to retry, with an idempotency key"
	case strings.Contains(line, "--format json"):
		return "JSON for agents"
	}
	var own []string
	for _, f := range flags {
		if f != "--format" && f != "--profile" {
			own = append(own, f)
		}
	}
	if len(own) == 0 {
		return "Basic use"
	}
	return "With " + strings.Join(own, ", ")
}

// exampleText renders a leaf's kit/examples as cobra Example text.
func exampleText(c *cobra.Command) string {
	ex, _ := cli.GetExamples(c)
	lines := make([]string, 0, len(ex))
	for _, e := range ex {
		lines = append(lines, "  "+e.Command)
	}
	return strings.Join(lines, "\n")
}
