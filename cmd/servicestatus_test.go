package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/bridge"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/hooks"
	"hop.top/heo/internal/storage"
	"hop.top/heo/internal/version"
	"hop.top/kit/go/console/cli"
)

func TestRuntimeSectionStates(t *testing.T) {
	st := daemon.Status{Services: []daemon.ServiceStatus{
		{Name: daemon.NameWorkers, State: daemon.StateNotStarted},
		{Name: daemon.NameReceivers, State: daemon.StateDegraded, Scopes: []daemon.ScopeStatus{
			{Scope: "channel:a", State: daemon.ScopeReady},
			{Scope: "channel:b", State: daemon.ScopeWaiting, Reason: "lease held"},
			{Scope: "channel:c", State: daemon.ScopeStarting},
			{Scope: "channel:d", State: daemon.ScopeFailed, Reason: "bind refused"},
		}},
		{Name: daemon.NameWebhook, State: daemon.StateFailed, Error: "listen: in use"},
	}}
	if sec := runtimeSection(st, daemon.NameWorkers, 1); sec.Status != cli.StatusEmpty {
		t.Errorf("not started = %+v, want empty", sec)
	}
	sec := runtimeSection(st, daemon.NameReceivers, 1)
	data, ok := sec.Data.(runtimeData)
	if sec.Status != cli.StatusDegraded || !ok || data.State != daemon.StateDegraded || len(data.Scopes) != 4 {
		t.Fatalf("degraded = %+v", sec)
	}
	for _, want := range []string{"channel:b=waiting (lease held)", "channel:c=starting", "channel:d=failed (bind refused)"} {
		if !strings.Contains(data.NotReady, want) {
			t.Errorf("not_ready = %q, lacks %q", data.NotReady, want)
		}
	}
	if strings.Contains(data.NotReady, "channel:a") {
		t.Errorf("not_ready lists a ready scope: %q", data.NotReady)
	}
	if sec := runtimeSection(st, daemon.NameWebhook, 1); sec.Status != cli.StatusError || sec.ErrorMessage != "failed: listen: in use" {
		t.Errorf("failed = %+v", sec)
	}
}

func TestStorageSectionStates(t *testing.T) {
	cases := []struct {
		in   daemon.StorageStatus
		want string
		msg  string
	}{
		{daemon.StorageStatus{State: daemon.StorageReady}, cli.StatusOK, ""},
		{daemon.StorageStatus{State: daemon.StorageClosed}, cli.StatusEmpty, ""},
		{daemon.StorageStatus{State: daemon.StorageLost, Error: "lease lost"}, cli.StatusError, "lost: lease lost"},
		{daemon.StorageStatus{State: daemon.StorageFailed}, cli.StatusError, "failed"},
	}
	for _, c := range cases {
		sec := storageSection(c.in)
		if sec.Status != c.want || sec.ErrorMessage != c.msg {
			t.Errorf("%s: %+v, want %s %q", c.in.State, sec, c.want, c.msg)
		}
	}
}

func TestQueueSectionStates(t *testing.T) {
	q := daemon.QueueStatus{Kind: "operation.execute", Waiting: 3, InFlight: 2, DeadLettered: 1}
	sec := queueSection(q, nil)
	if sec.Status != cli.StatusOK || sec.Title != queueStatusName || sec.Data != q {
		t.Errorf("open = %+v", sec)
	}
	if sec := queueSection(daemon.QueueStatus{}, daemon.ErrNotRunning); sec.Status != cli.StatusEmpty || sec.Data != nil {
		t.Errorf("not running = %+v, want empty", sec)
	}
	sec = queueSection(daemon.QueueStatus{}, errors.New("postgres: connection refused"))
	if sec.Status != cli.StatusError || sec.ErrorMessage != "unreadable: postgres: connection refused" || sec.Data != nil {
		t.Errorf("failed read = %+v", sec)
	}
}

func TestOwnerSectionStates(t *testing.T) {
	o := &daemon.OwnerStatus{Owner: "heo/default", PID: 42, Host: "box", Since: time.Unix(1, 0).UTC()}
	sec := ownerSection(daemon.Status{Storage: daemon.StorageStatus{State: daemon.StorageReady}, Owner: o})
	if sec.Status != cli.StatusOK || sec.Title != ownerStatusName || sec.Data != o {
		t.Errorf("owned = %+v", sec)
	}
	if sec := ownerSection(daemon.Status{Storage: daemon.StorageStatus{State: daemon.StorageClosed}}); sec.Status != cli.StatusEmpty {
		t.Errorf("closed = %+v, want empty", sec)
	}
	sec = ownerSection(daemon.Status{Storage: daemon.StorageStatus{State: daemon.StorageLost, Error: "session gone"}, Owner: o})
	if sec.Status != cli.StatusError || sec.ErrorMessage != "ownership lost: session gone" || sec.Data != nil {
		t.Errorf("lost = %+v", sec)
	}
}

func TestVersionsSection(t *testing.T) {
	rep := &storage.Report{
		Versions: storage.Versions{Driver: "postgres", Library: "hop.top/uma/store/postgres@v1", Schema: 7, LibrarySchema: 7},
		Compat:   storage.CompatOK,
	}
	sec := versionsSection(daemon.Status{Storage: daemon.StorageStatus{State: daemon.StorageReady, Report: rep}})
	d, ok := sec.Data.(versionsData)
	if sec.Status != cli.StatusOK || sec.Title != versionsStatusName || !ok {
		t.Fatalf("versions = %+v", sec)
	}
	want := versionsData{
		Heo: version.Version(), Kit: version.Module("hop.top/kit"), Uma: version.Module("hop.top/uma"),
		Store: rep.Library, Schema: 7, LibrarySchema: 7, Compat: storage.CompatOK,
	}
	if d != want {
		t.Errorf("versions = %+v, want %+v", d, want)
	}
	// No store opened: the binary's versions alone.
	d, _ = versionsSection(daemon.Status{Storage: daemon.StorageStatus{State: daemon.StorageClosed}}).Data.(versionsData)
	if d.Heo == "" || d.Kit == "" || d.Uma == "" || d.Store != "" || d.Schema != 0 || d.Compat != "" {
		t.Errorf("versions without a store = %+v", d)
	}
}

// TestBridgeSectionStates: the event bridge's section is empty when the
// run did not start it, an error when it failed, and otherwise names
// its subscription, the last event it published, the count and the
// drops; while it retries it says why. None carries an event's content.
func TestBridgeSectionStates(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	st := daemon.Status{
		Services: []daemon.ServiceStatus{{Name: daemon.NameBridge, State: daemon.StateReady, Scopes: []daemon.ScopeStatus{
			{Scope: daemon.BridgeScope, State: daemon.ScopeReady},
		}}},
		Bridge: &daemon.BridgeStatus{Status: bridge.Status{
			Consumer: bridge.Consumer, State: bridge.StateReady, Checkpoint: "cp-7", LastEvent: "ev-9", LastEventAt: at, Published: 9,
		}, Dropped: 2},
	}
	sec := bridgeSection(st)
	data, ok := sec.Data.(bridgeData)
	if sec.Status != cli.StatusOK || sec.Title != daemon.NameBridge || !ok {
		t.Fatalf("ready = %+v", sec)
	}
	want := bridgeData{State: daemon.StateReady, Consumer: bridge.Consumer, Checkpoint: "cp-7", LastEvent: "ev-9", LastEventAt: at, Published: 9, Dropped: 2}
	if data != want {
		t.Errorf("data = %+v, want %+v", data, want)
	}

	st.Services[0].State = daemon.StateDegraded
	st.Services[0].Scopes[0] = daemon.ScopeStatus{Scope: daemon.BridgeScope, State: daemon.ScopeWaiting, Reason: "retrying after unavailable"}
	data, _ = bridgeSection(st).Data.(bridgeData)
	if data.State != daemon.StateDegraded || !strings.Contains(data.NotReady, "retrying after unavailable") {
		t.Errorf("degraded = %+v", data)
	}

	if sec := bridgeSection(daemon.Status{Services: []daemon.ServiceStatus{{Name: daemon.NameBridge, State: daemon.StateNotStarted}}}); sec.Status != cli.StatusEmpty {
		t.Errorf("not started = %+v", sec)
	}
	failed := daemon.Status{Services: []daemon.ServiceStatus{{Name: daemon.NameBridge, State: daemon.StateFailed, Error: "store ownership lost"}}}
	if sec := bridgeSection(failed); sec.Status != cli.StatusError || sec.ErrorMessage != "failed: store ownership lost" {
		t.Errorf("failed = %+v", sec)
	}
}

// TestHooksSectionStates: the hook runner's section is empty when the run
// did not start it, an error when it failed, and otherwise lists each
// plugin with its subscription, cursor, lag and last error; a plugin that
// retries is named in not_ready. No event content is in it.
func TestHooksSectionStates(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	st := daemon.Status{
		Services: []daemon.ServiceStatus{{Name: daemon.NameHooks, State: daemon.StateDegraded, Scopes: []daemon.ScopeStatus{
			{Scope: "plugin:triage", State: daemon.ScopeReady},
			{Scope: "plugin:fwd", State: daemon.ScopeWaiting, Reason: "retrying after unavailable"},
		}}},
		Hooks: []hooks.Status{{
			Plugin: "triage", Consumer: "plugin-triage-0123456789ab", Mode: "exec", State: hooks.StateReady, Cursor: "cp-7",
			Lag: 90*time.Second + 400*time.Millisecond, InFlight: 2, Delivered: 9, DeadLettered: 1,
			LastEvent: "ev-9", LastError: &hooks.LastError{Event: "ev-8", At: at, Message: "exit 1"},
		}, {Plugin: "fwd", Consumer: "plugin-fwd-ba9876543210", Mode: "stream", State: hooks.StateDegraded}},
	}
	sec := hooksSection(st)
	data, ok := sec.Data.(hooksData)
	if sec.Status != cli.StatusDegraded || sec.Title != daemon.NameHooks || !ok || len(data.Plugins) != 2 {
		t.Fatalf("section = %+v", sec)
	}
	p := data.Plugins[0]
	if p.Plugin != "triage" || p.Cursor != "cp-7" || p.Lag != "1m30s" || p.InFlight != 2 || p.Delivered != 9 || p.DeadLettered != 1 ||
		p.LastError == nil || p.LastError.Message != "exit 1" || p.LastError.Event != "ev-8" || p.LastEvent != "ev-9" {
		t.Errorf("plugin = %+v", p)
	}
	if data.Plugins[1].Lag != "" || data.State != daemon.StateDegraded || !strings.Contains(data.NotReady, "plugin:fwd=waiting") {
		t.Errorf("data = %+v", data)
	}

	if sec := hooksSection(daemon.Status{Services: []daemon.ServiceStatus{{Name: daemon.NameHooks, State: daemon.StateNotStarted}}}); sec.Status != cli.StatusEmpty {
		t.Errorf("not started = %+v", sec)
	}
	failed := daemon.Status{Services: []daemon.ServiceStatus{{Name: daemon.NameHooks, State: daemon.StateFailed, Error: "store ownership lost"}}}
	if sec := hooksSection(failed); sec.Status != cli.StatusError || sec.ErrorMessage != "failed: store ownership lost" {
		t.Errorf("failed = %+v", sec)
	}
}

// TestDegradedReceiversSurviveTheHumanTable: a receivers service with
// failed channels reads degraded, not ok; its channels are named by key
// (the id when it has none), and the table's DETAIL cell is a short
// digest, so kit does not drop the column.
func TestDegradedReceiversSurviveTheHumanTable(t *testing.T) {
	scopes := []daemon.ScopeStatus{
		{Scope: "channel:11111111-aaaa", Key: "gm-000", State: daemon.ScopeFailed, Reason: "no credential"},
		{Scope: "channel:22222222-bbbb", Key: "ms-000", State: daemon.ScopeFailed, Reason: "no credential"},
		{Scope: "channel:33333333-cccc", Key: "gm-001", State: daemon.ScopeWaiting, Reason: "lease held"},
		{Scope: "channel:44444444-dddd", Key: "gm-002", State: daemon.ScopeFailed, Reason: "no credential"},
		{Scope: "channel:55555555-eeee", Key: "gm-003", State: daemon.ScopeFailed, Reason: "no credential"},
		{Scope: "channel:66666666-ffff", State: daemon.ScopeFailed, Reason: "no credential"},
	}
	for i := range 20 {
		scopes = append(scopes, daemon.ScopeStatus{Scope: fmt.Sprintf("channel:ok-%d", i), Key: fmt.Sprintf("ok-%02d", i), State: daemon.ScopeReady})
	}
	st := daemon.Status{Services: []daemon.ServiceStatus{{Name: daemon.NameReceivers, State: daemon.StateDegraded, Scopes: scopes}}}
	sec := runtimeSection(st, daemon.NameReceivers, 1)
	if sec.Status != cli.StatusDegraded {
		t.Fatalf("status = %q, want %q", sec.Status, cli.StatusDegraded)
	}
	data := sec.Data.(runtimeData)
	if strings.Contains(data.NotReady, "11111111") || !strings.Contains(data.NotReady, "gm-000=failed (no credential)") ||
		!strings.Contains(data.NotReady, "channel:66666666-ffff=failed") {
		t.Errorf("not_ready names channels by uuid: %q", data.NotReady)
	}
	rows := cli.StatusSectionRows([]cli.StatusSection{sec})
	d := rows[0].Detail
	if rows[0].Status != cli.StatusDegraded {
		t.Errorf("row status = %q", rows[0].Status)
	}
	for _, want := range []string{"6/26", "gm-000", "ms-000"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail %q lacks %q", d, want)
		}
	}
	if len(d) > 100 || strings.Contains(d, "ok-") {
		t.Errorf("detail not a short digest of the failures: %q (%d bytes)", d, len(d))
	}
}

// TestHealthyRuntimeSectionIsUnchanged: every scope ready stays ok with
// no digest.
func TestHealthyRuntimeSectionIsUnchanged(t *testing.T) {
	st := daemon.Status{Services: []daemon.ServiceStatus{{Name: daemon.NameReceivers, State: daemon.StateReady,
		Scopes: []daemon.ScopeStatus{{Scope: "channel:a", Key: "k", State: daemon.ScopeReady}}}}}
	sec := runtimeSection(st, daemon.NameReceivers, 1)
	if sec.Status != cli.StatusOK || sec.ErrorMessage != "" || sec.Data.(runtimeData).NotReady != "" {
		t.Errorf("healthy = %+v", sec)
	}
}

// TestDegradedLinkReceiverIsInTheDigest: a channel that receives by
// polling while its adapter reports a degraded link makes the section
// degraded, counted apart from the waits in the short digest; the json
// scope carries the code.
func TestDegradedLinkReceiverIsInTheDigest(t *testing.T) {
	scopes := []daemon.ScopeStatus{
		{Scope: "channel:a", Key: "gm-000", State: daemon.ScopeDegraded, Code: "google.mail.push_denied",
			Reason: "push registration refused (google.mail.push_denied): mail arrives by polling only"},
		{Scope: "channel:b", Key: "gm-001", State: daemon.ScopeWaiting, Reason: "lease held"},
		{Scope: "channel:c", Key: "gm-002", State: daemon.ScopeReady},
	}
	st := daemon.Status{Services: []daemon.ServiceStatus{{Name: daemon.NameReceivers, State: daemon.StateDegraded, Scopes: scopes}}}
	sec := runtimeSection(st, daemon.NameReceivers, 1)
	if sec.Status != cli.StatusDegraded || sec.ErrorMessage != "2/3 not ready (waiting 1, degraded 1): gm-000, gm-001" {
		t.Errorf("section = %q %q", sec.Status, sec.ErrorMessage)
	}
	if nr := sec.Data.(runtimeData).NotReady; !strings.Contains(nr, "gm-000=degraded (push registration refused (google.mail.push_denied)") {
		t.Errorf("not_ready = %q", nr)
	}
	b, err := json.Marshal(sec.Data)
	if err != nil || !strings.Contains(string(b), `"code":"google.mail.push_denied"`) {
		t.Errorf("json = %s (%v)", b, err)
	}
}

// TestDigestCountsDegradedScopesByState: failed and degraded scopes are
// counted by their own state, worst first, and a section whose scopes
// still receive is degraded by kit's status value, not a string of ours.
func TestDigestCountsDegradedScopesByState(t *testing.T) {
	var scopes []daemon.ScopeStatus
	for i := range 2 {
		scopes = append(scopes, daemon.ScopeStatus{Scope: fmt.Sprintf("channel:f%d", i), Key: fmt.Sprintf("f-%d", i), State: daemon.ScopeFailed})
	}
	for i := range 3 {
		scopes = append(scopes, daemon.ScopeStatus{Scope: fmt.Sprintf("channel:d%d", i), Key: fmt.Sprintf("d-%d", i), State: daemon.ScopeDegraded, Code: "google.mail.push_denied"})
	}
	st := daemon.Status{Services: []daemon.ServiceStatus{{Name: daemon.NameReceivers, State: daemon.StateDegraded, Scopes: scopes}}}
	sec := runtimeSection(st, daemon.NameReceivers, 1)
	if sec.Status != cli.StatusDegraded || sec.ErrorMessage != "5/5 not ready (failed 2, degraded 3): f-0, f-1, d-0 +2 more" {
		t.Errorf("section = %q %q", sec.Status, sec.ErrorMessage)
	}
	if cli.StatusSeverity(sec.Status) != cli.StatusSeverity(cli.StatusDegraded) {
		t.Errorf("severity of %q", sec.Status)
	}
}
