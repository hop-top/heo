package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/model"
)

const testModal = `{
  "type": "modal",
  "title": {"type": "plain_text", "text": "Approve"},
  "blocks": []
}`

// modalLeaf parses args on the modal verb at path, in a terminal's
// process, with standard input in.
func modalLeaf(t *testing.T, path, in string, args ...string) (*app, *cobra.Command) {
	t.Helper()
	a := newApp()
	c := find(a.r.Cmd, path)
	if c == nil {
		t.Fatalf("%s: not in the tree", path)
	}
	if err := c.ParseFlags(args); err != nil {
		t.Fatalf("%s %v: %v", path, args, err)
	}
	c.SetContext(context.Background())
	c.SetIn(strings.NewReader(in))
	return a, c
}

func writeView(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "view.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestModalViewIsReadWhereTheCallerIs: in the process the caller typed the
// command in, --view is a file, standard input or the JSON itself; the
// daemon has neither the caller's directory nor its input, so the
// compact view replaces the flag's value, which is what is forwarded.
func TestModalViewIsReadWhereTheCallerIs(t *testing.T) {
	compact := `{"type":"modal","title":{"type":"plain_text","text":"Approve"},"blocks":[]}`
	file := writeView(t, testModal)
	for name, tc := range map[string]struct {
		flag, stdin string
	}{
		"a file":         {flag: file},
		"standard input": {flag: "-", stdin: testModal},
		"inline":         {flag: compact},
		"inline, padded": {flag: "  " + testModal + "\n"},
	} {
		a, c := modalLeaf(t, "interaction open-modal", tc.stdin, "--view", tc.flag)
		got, err := a.modalView(c)
		if err != nil || string(got) != compact {
			t.Errorf("%s: view %s, %v; want it compact", name, got, err)
		}
		if v, _ := c.Flags().GetString(flagView); v != compact {
			t.Errorf("%s: the flag forwarded is %q, want the compact view", name, v)
		}
		if sent := control.RequestFor(c, []string{"ix_1"}); sent.Flags[flagView] != compact {
			t.Errorf("%s: the daemon is sent %v", name, sent.Flags[flagView])
		}
	}
}

// TestModalViewRefusals: a view that cannot be read, is not a JSON
// object or is larger than uma bounds it is a usage error (exit 2)
// before the daemon is reached, and the refusal never repeats the view.
func TestModalViewRefusals(t *testing.T) {
	big := `{"type":"modal","title":{"type":"plain_text","text":"T"},"blocks":[],"private_metadata":"` + strings.Repeat("s", slack.MaxViewBytes) + `"}`
	for name, tc := range map[string]struct {
		flag, stdin, want string
	}{
		"no such file":      {flag: filepath.Join(t.TempDir(), "nope.json"), want: "no such file"},
		"a directory":       {flag: t.TempDir(), want: "--view"},
		"empty stdin":       {flag: "-", stdin: "", want: "not valid JSON"},
		"not JSON":          {flag: "-", stdin: "type: modal", want: "not valid JSON"},
		"an array":          {flag: "-", stdin: "[]", want: "JSON object"},
		"a string":          {flag: "-", stdin: `"x"`, want: "JSON object"},
		"over the bound":    {flag: "-", stdin: big, want: "250 KiB"},
		"far over the read": {flag: "-", stdin: strings.Repeat(" ", viewReadLimit+1), want: "250 KiB"},
		"a blank flag":      {flag: "  ", want: "--view"},
	} {
		a, c := modalLeaf(t, "interaction open-modal", tc.stdin, "--view", tc.flag)
		_, err := a.modalView(c)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a usage error containing %q", name, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), "sssss") {
			t.Errorf("%s: the refusal repeats the view", name)
		}
		if got := exitOf(err); got != 2 {
			t.Errorf("%s: exit %d, want 2", name, got)
		}
	}
	// A pretty-printed view whose compact form fits is not refused for its
	// indentation: uma bounds the compact view.
	pad := slack.MaxViewBytes - len(`{"type":"modal","title":{"type":"plain_text","text":"T"},"blocks":[],"private_metadata":""}`)
	tight := `{"type":"modal","title":{"type":"plain_text","text":"T"},"blocks":[],"private_metadata":"` + strings.Repeat("s", pad) + `"}`
	indented := strings.Replace(tight, `"blocks"`, "\n    \"blocks\"", 1) + strings.Repeat("\n", 1000)
	a, c := modalLeaf(t, "interaction open-modal", indented, "--view", "-")
	if got, err := a.modalView(c); err != nil || len(got) != slack.MaxViewBytes {
		t.Errorf("a view of exactly the bound once compact: %d bytes, %v", len(got), err)
	}
}

// TestModalViewInTheDaemonIsTheView: a call the daemon serves (the
// forwarded one, REST, MCP) carries the view as the flag's value; the daemon
// never reads a file or its own standard input for it.
func TestModalViewInTheDaemonIsTheView(t *testing.T) {
	file := writeView(t, testModal)
	_, c := modalLeaf(t, "interaction open-modal", "", "--view", file)
	_, err := modalViewAt(c, control.Local)
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("a file name in the daemon: err = %v, want it taken as the view and refused", err)
	}
	_, c = modalLeaf(t, "interaction open-modal", "", "--view", `{"type":"modal","title":{"type":"plain_text","text":"T"},"blocks":[]}`)
	if _, err := modalViewAt(c, control.Local); err != nil {
		t.Errorf("the view itself in the daemon: %v", err)
	}
}

// TestUpdateModalFlags: the view is named by its ID or its external_id,
// one of them, in an inbox; the view is always given.
func TestUpdateModalFlags(t *testing.T) {
	for args, wantErr := range map[string]bool{
		"--inbox rooms --view-id V1 --view v.json":                 false,
		"--inbox rooms --external-id deploy-42 --view v.json":      false,
		"--inbox rooms --view-id V1 --hash 1.2 --view v.json":      false,
		"--inbox rooms --view-id V1 --external-id x --view v.json": true,
		"--inbox rooms --view v.json":                              true,
		"--view-id V1 --view v.json":                               true,
		"--inbox rooms --view-id V1":                               true,
		"--inbox rooms --hash 1.2 --view v.json":                   true,
	} {
		c := find(newApp().r.Cmd, "interaction update-modal")
		if err := c.ParseFlags(strings.Fields(args)); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		err := c.ValidateRequiredFlags()
		if err == nil {
			err = c.ValidateFlagGroups()
		}
		if (err != nil) != wantErr {
			t.Errorf("%q: err = %v, want an error: %v", args, err, wantErr)
		}
	}
}

// TestModalVerbsAreSharedWrites: the three verbs reach a provider, are
// idempotent under a key, and are provider-dependent on
// extension_execute, which `interaction respond` is not.
func TestModalVerbsAreSharedWrites(t *testing.T) {
	a := newApp()
	for _, path := range []string{"interaction open-modal", "interaction push-modal", "interaction update-modal"} {
		c := find(a.r.Cmd, path)
		if c == nil {
			t.Fatalf("%s: not in the tree", path)
		}
		if got, _ := cli.GetSideEffect(c); got != cli.SideEffectWriteShared {
			t.Errorf("%s: side effect %q, want write-shared", path, got)
		}
		if got := c.Annotations[annotationCapability]; got != string(model.CapabilityExtensionExecute) {
			t.Errorf("%s: capability %q", path, got)
		}
		if !control.IsDaemon(c) {
			t.Errorf("%s: not daemon-backed", path)
		}
	}
	if got := find(a.r.Cmd, "interaction respond").Annotations[annotationCapability]; got != string(model.CapabilityRespond) {
		t.Errorf("interaction respond needs %q, not the modal verbs' capability", got)
	}
}

// TestModalOutputNeverShowsTheTrigger: the trigger_id is Slack's
// single-use credential; neither the output nor the plan carries it, and
// the output says where the opened view's ID will be and how long the
// trigger lives.
func TestModalOutputNeverShowsTheTrigger(t *testing.T) {
	until := time.Date(2026, 10, 15, 12, 0, 3, 0, time.UTC)
	ix := model.Interaction{Ref: model.InteractionRef{ID: "ix_1", Conversation: model.ConversationRef{ID: "conv_1", Inbox: model.InboxRef{ID: "in_1"}}}}
	ix.Actor.Identity.ID = "idn_1"
	data, _ := json.Marshal(map[string]any{"trigger_id": "398738663015.47445629121.secret", "expires_at": until, "view": json.RawMessage(`{}`)})
	p := desk.ModalPlan{
		Request: model.ExecuteExtension{Audience: model.Audience{Visibility: model.Internal}, Action: model.Extension{Name: slack.ExtensionViewsOpen, Version: 1, Data: data}},
		Effect:  slack.ExtensionViewsOpen, Inbox: model.Inbox{Ref: model.InboxRef{ID: "in_1"}}, Interaction: ix, ExpiresAt: until, ExternalID: "deploy-42",
	}
	acc := model.Acceptance{Durable: true, Operation: model.Operation{Ref: model.OperationRef{ID: "op_9"}, State: model.Queued}}
	out := modalOutOf(p, acc, "key-1")
	b, _ := json.Marshal(out)
	plan, _ := json.Marshal(modalPlan(p, "key-1"))
	for name, got := range map[string]string{"output": string(b), "plan": string(plan), "rows": strings.Join(flatten(modalRows(out)), "\n"), "notes": strings.Join(out.Notes, "\n")} {
		if strings.Contains(got, "secret") || strings.Contains(got, "398738663015") {
			t.Errorf("the %s shows the trigger_id: %s", name, got)
		}
	}
	notes := strings.Join(out.Notes, "\n")
	for _, want := range []string{"2026-10-15T12:00:03Z", "heo operation get op_9", "provider reference", "--view-id <id>", "--inbox in_1", "--external-id deploy-42", "never sent again"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes lack %q:\n%s", want, notes)
		}
	}
	if v := rowOf(modalRows(out), "trigger valid until"); v != "2026-10-15T12:00:03Z" {
		t.Errorf("trigger valid until = %q", v)
	}
	if rowOf(modalRows(out), "external id") != "deploy-42" {
		t.Errorf("rows = %v", modalRows(out))
	}

	// An update shows the view it names, spends no trigger, and says no window.
	up := desk.ModalPlan{Effect: slack.ExtensionViewsUpdate, Inbox: model.Inbox{Ref: model.InboxRef{ID: "in_1"}}, View: desk.ViewRef{ViewID: "V0123456789", Hash: "1.2"}}
	uo := modalOutOf(up, acc, "key-2")
	if uo.TriggerUntil != nil || uo.Interaction != "" || rowOf(modalRows(uo), "view") != "V0123456789" || rowOf(modalRows(uo), "hash") != "1.2" {
		t.Errorf("update output = %+v rows %v", uo, modalRows(uo))
	}
	if strings.Contains(strings.Join(uo.Notes, "\n"), "trigger_id") {
		t.Errorf("an update's notes speak of a trigger_id: %v", uo.Notes)
	}
}

func flatten(rows [][2]string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r[0] + " " + r[1]
	}
	return out
}

// exitOf is the exit code of a command error.
func exitOf(err error) int {
	var oe *output.Error
	if errors.As(err, &oe) {
		return oe.ExitCode
	}
	return -1
}

// TestDeskIsWiredForModals: the desk a daemon command gets reads an
// interaction's trigger the adapter's way and wakes the workers as a modal
// is queued; without them a modal would sit out the workers' poll, or read
// no trigger.
func TestDeskIsWiredForModals(t *testing.T) {
	s := deskOf(daemon.Runtime{}, nil, nil, control.Caller{Actor: "uid:1", Tenant: "default"})
	if s.Kick == nil {
		t.Error("the desk does not wake the workers")
	}
	if s.Trigger == nil || s.Effects == nil || s.ValidateAction == nil || s.Adapter == nil {
		t.Errorf("the desk lacks a hook a modal needs: trigger %v effects %v validate %v adapter %v",
			s.Trigger != nil, s.Effects != nil, s.ValidateAction != nil, s.Adapter != nil)
	}
	s.Kick() // a runtime with no workers to wake is not an error
	in := model.Interaction{Source: slack.Type, Deadline: time.Date(2026, 10, 15, 12, 0, 3, 0, time.UTC)}
	in.External = []model.ExternalRef{{Namespace: model.ProviderMessageNamespace, Kind: model.RefKindInteraction, ID: "action:398738663015.47445629121.aaaa"}}
	if trig, ok := s.Trigger(in); !ok || trig.ID != "398738663015.47445629121.aaaa" || !trig.ExpiresAt.Equal(in.Deadline) {
		t.Errorf("trigger = %+v, %v", trig, ok)
	}
	in.Source = "other.chat"
	if _, ok := s.Trigger(in); ok {
		t.Error("another adapter's interaction has a Slack trigger")
	}
}
