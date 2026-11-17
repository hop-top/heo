package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/hooks"
	"hop.top/heo/internal/settings"
	"hop.top/heo/internal/storage"
	"hop.top/heo/internal/version"
	"hop.top/kit/go/console/cli"
)

// Status section names and priorities. Kit's own root status sections
// occupy 100-600; heo's start at 1000.
const (
	profileStatusName  = "heo-profile"
	daemonStatusName   = "heo"
	ownerStatusName    = "store-owner"
	queueStatusName    = "queue"
	versionsStatusName = "versions"

	priorityProfile = 1000
	priorityDaemon  = 1000
	priorityStorage = 1010
	priorityOwner   = 1011
	priorityRuntime = 1020
	priorityBridge  = 1023
	// priorityChat follows the bridge.
	priorityChat     = 1024
	priorityHooks    = 1025
	priorityQueue    = 1030
	priorityVersions = 1040
)

// profileStatus is root `status`'s heo section: the profile this
// invocation selected and where its configuration and control socket
// live. It reads the loaded configuration only; no daemon is asked.
func (a *app) profileStatus(context.Context) (cli.StatusSection, error) {
	sec := cli.StatusSection{Title: profileStatusName, Priority: priorityProfile}
	if a.state == nil {
		return sec, errors.New("configuration not loaded")
	}
	ref := a.state.Profile()
	sec.Data = map[string]string{
		"profile": ref.String(),
		"dir":     ref.Dir(),
		"file":    a.state.ProfilePath(),
		"socket":  a.r.Viper.GetString(settings.KeySocketPath),
	}
	return sec, nil
}

// registerServiceStatus adds heo's sections to kit's `service status`
// (and to the report `service restart` prints): the daemon's profile,
// its data store and who owns it, each runtime service with its scopes
// — a channel for receivers and webhook, a worker for workers — and
// where each stands (starting, ready, waiting, failed), the operation
// queue's depth, and the versions it runs. Kit runs them in the daemon,
// so they report the daemon's state, not the caller's. None carries a
// connection string, credential or credential reference.
func (a *app) registerServiceStatus() {
	a.r.RegisterServiceStatusProvider(daemonStatusName, a.daemonSection)
	a.r.RegisterServiceStatusProvider(storage.StatusTitle, func(context.Context) (cli.StatusSection, error) {
		return storageSection(a.runtime.Status().Storage), nil
	})
	a.r.RegisterServiceStatusProvider(ownerStatusName, func(context.Context) (cli.StatusSection, error) {
		return ownerSection(a.runtime.Status()), nil
	})
	a.r.RegisterServiceStatusProvider(queueStatusName, func(ctx context.Context) (cli.StatusSection, error) {
		q, err := a.runtime.Queue(ctx)
		return queueSection(q, err), nil
	})
	a.r.RegisterServiceStatusProvider(versionsStatusName, func(context.Context) (cli.StatusSection, error) {
		return versionsSection(a.runtime.Status()), nil
	})
	for i, name := range []string{daemon.NameWorkers, daemon.NameReceivers, daemon.NameWebhook} {
		prio := priorityRuntime + i
		a.r.RegisterServiceStatusProvider(name, func(context.Context) (cli.StatusSection, error) {
			return runtimeSection(a.runtime.Status(), name, prio), nil
		})
	}
	a.r.RegisterServiceStatusProvider(daemon.NameApprovalChat, func(context.Context) (cli.StatusSection, error) {
		return runtimeSection(a.runtime.Status(), daemon.NameApprovalChat, priorityChat), nil
	})
	a.r.RegisterServiceStatusProvider(daemon.NameBridge, func(context.Context) (cli.StatusSection, error) {
		return bridgeSection(a.runtime.Status()), nil
	})
	a.r.RegisterServiceStatusProvider(daemon.NameHooks, func(context.Context) (cli.StatusSection, error) {
		return hooksSection(a.runtime.Status()), nil
	})
}

// daemonSection names the profile, tenant and version the daemon runs.
func (a *app) daemonSection(context.Context) (cli.StatusSection, error) {
	sec := cli.StatusSection{Title: daemonStatusName, Priority: priorityDaemon}
	if a.state == nil {
		return sec, errors.New("configuration not loaded")
	}
	sec.Data = map[string]any{
		"profile": a.state.Profile().String(),
		"tenant":  a.r.Viper.GetString(settings.KeyTenant),
		"version": version.Version(),
		"pid":     os.Getpid(),
	}
	return sec, nil
}

// storageSection is the data store's state and, while it is open, its
// versions and their compatibility with this binary.
func storageSection(st daemon.StorageStatus) cli.StatusSection {
	sec := cli.StatusSection{Title: storage.StatusTitle, Priority: priorityStorage, Data: st}
	switch st.State {
	case daemon.StorageReady:
		sec.Status = cli.StatusOK
	case daemon.StorageClosed:
		// No runtime service opened it in this run.
		sec.Status = cli.StatusEmpty
		sec.Data = nil
	default:
		sec.Status = cli.StatusError
		sec.ErrorMessage = string(st.State)
		if st.Error != "" {
			sec.ErrorMessage += ": " + st.Error
		}
		sec.Data = nil
	}
	return sec
}

// ownerSection names who holds the data store's ownership: this
// daemon, by owner name, pid and host, since when, and when that was
// last verified. Empty while no runtime service holds the store; an
// error once ownership was lost.
func ownerSection(st daemon.Status) cli.StatusSection {
	sec := cli.StatusSection{Title: ownerStatusName, Priority: priorityOwner}
	switch {
	case st.Storage.State == daemon.StorageLost:
		sec.Status = cli.StatusError
		sec.ErrorMessage = "ownership lost"
		if st.Storage.Error != "" {
			sec.ErrorMessage += ": " + st.Storage.Error
		}
	case st.Owner == nil:
		sec.Status = cli.StatusEmpty
	default:
		sec.Status = cli.StatusOK
		sec.Data = st.Owner
	}
	return sec
}

// queueSection is the operation queue's depth: items waiting, in
// flight and dead-lettered. Empty while no runtime service holds the
// store; an error when the store did not answer.
func queueSection(q daemon.QueueStatus, err error) cli.StatusSection {
	sec := cli.StatusSection{Title: queueStatusName, Priority: priorityQueue}
	switch {
	case errors.Is(err, daemon.ErrNotRunning):
		sec.Status = cli.StatusEmpty
	case err != nil:
		sec.Status = cli.StatusError
		sec.ErrorMessage = "unreadable: " + err.Error()
	default:
		sec.Status = cli.StatusOK
		sec.Data = q
	}
	return sec
}

// versionsData is the versions section's payload.
type versionsData struct {
	// Heo, Kit and Uma are this binary's version and the kit and uma
	// modules it was built with.
	Heo string `json:"heo" yaml:"heo"`
	Kit string `json:"kit" yaml:"kit"`
	Uma string `json:"uma" yaml:"uma"`
	// Store is the store package and its version; Schema the schema
	// version the store holds, LibrarySchema the one this binary
	// implements, Compat their compatibility. Unset until a runtime
	// service has opened the store.
	Store         string         `json:"store,omitempty" yaml:"store,omitempty"`
	Schema        int64          `json:"schema,omitempty" yaml:"schema,omitempty"`
	LibrarySchema int64          `json:"library_schema,omitempty" yaml:"library_schema,omitempty"`
	Compat        storage.Compat `json:"compat,omitempty" yaml:"compat,omitempty"`
}

// versionsSection is the versions the daemon runs: heo, kit and uma,
// and, once the store was opened, its schema version against this
// binary's and their compatibility.
func versionsSection(st daemon.Status) cli.StatusSection {
	d := versionsData{
		Heo: version.Version(),
		Kit: version.Module("hop.top/kit"),
		Uma: version.Module("hop.top/uma"),
	}
	if rep := st.Storage.Report; rep != nil {
		d.Store, d.Schema, d.LibrarySchema, d.Compat = rep.Library, rep.Schema, rep.LibrarySchema, rep.Compat
	}
	return cli.StatusSection{Title: versionsStatusName, Priority: priorityVersions, Status: cli.StatusOK, Data: d}
}

// runtimeSection is one runtime service: its state, its listener, and
// each scope's state. A service this run did not start is empty; one
// that failed is an error; one with a scope that is not ready
// (starting, waiting, degraded or failed) is degraded, and names those scopes.
func runtimeSection(all daemon.Status, name string, prio int) cli.StatusSection {
	sec := cli.StatusSection{Title: name, Priority: prio}
	var s daemon.ServiceStatus
	for _, ss := range all.Services {
		if ss.Name == name {
			s = ss
		}
	}
	switch s.State {
	case "", daemon.StateNotStarted:
		sec.Status = cli.StatusEmpty
		return sec
	case daemon.StateFailed:
		sec.Status = cli.StatusError
		sec.ErrorMessage = "failed"
		if s.Error != "" {
			sec.ErrorMessage += ": " + s.Error
		}
		return sec
	}
	sec.Status = cli.StatusOK
	// A section that runs with a scope not ready is degraded in kit's
	// sense: it serves, so `service status` still exits 0.
	nr := notReady(s.Scopes)
	if nr != "" || s.State == daemon.StateDegraded {
		sec.Status = cli.StatusDegraded
		sec.ErrorMessage = notReadyDigest(s.Scopes)
	}
	sec.Data = runtimeData{
		State:    s.State,
		Address:  s.Address,
		Scopes:   s.Scopes,
		NotReady: nr,
	}
	return sec
}

// bridgeData is the event bridge section's payload.
type bridgeData struct {
	State daemon.ServiceState `json:"state" yaml:"state"`
	// Consumer is the bridge's subscription in uma, apart from any
	// `event watch` consumer.
	Consumer string `json:"consumer" yaml:"consumer"`
	// Checkpoint is the subscription's last acknowledged position.
	Checkpoint string `json:"checkpoint,omitempty" yaml:"checkpoint,omitempty"`
	// LastEvent and LastEventAt are the last event published and when
	// uma recorded it; Published counts the events published since the
	// bridge started, Dropped those dropped for bus subscribers that
	// fell behind. The distance to the log head is not known cheaply, so
	// a bridge that keeps up shows a recent last event.
	LastEvent   string    `json:"last_event,omitempty" yaml:"last_event,omitempty"`
	LastEventAt time.Time `json:"last_event_at,omitzero" yaml:"last_event_at,omitempty"`
	Published   uint64    `json:"published" yaml:"published"`
	Dropped     uint64    `json:"dropped" yaml:"dropped"`
	// NotReady names why the bridge is degraded, as in the other
	// runtime services.
	NotReady string `json:"not_ready,omitempty" yaml:"not_ready,omitempty"`
}

// bridgeSection is the event bridge: empty when this run did not start
// it, an error when it failed, otherwise its subscription, the last
// event it published and, while it retries, why.
func bridgeSection(all daemon.Status) cli.StatusSection {
	sec := runtimeSection(all, daemon.NameBridge, priorityBridge)
	rt, ok := sec.Data.(runtimeData)
	if !ok || all.Bridge == nil {
		return sec
	}
	b := all.Bridge
	sec.Data = bridgeData{
		State: rt.State, Consumer: b.Consumer, Checkpoint: string(b.Checkpoint), LastEvent: string(b.LastEvent),
		LastEventAt: b.LastEventAt, Published: b.Published, Dropped: b.Dropped, NotReady: rt.NotReady,
	}
	return sec
}

// hooksData is the hook runner section's payload: one entry per
// plugin.
type hooksData struct {
	State   daemon.ServiceState `json:"state" yaml:"state"`
	Plugins []hookData          `json:"plugins" yaml:"plugins"`
	// NotReady names the plugins that are retrying, as in the other
	// runtime services.
	NotReady string `json:"not_ready,omitempty" yaml:"not_ready,omitempty"`
}

// hookData is one plugin's subscription: where it stands in the log
// (cursor and lag), what it did and what failed last.
type hookData struct {
	Plugin   string `json:"plugin" yaml:"plugin"`
	Consumer string `json:"consumer" yaml:"consumer"`
	Mode     string `json:"mode" yaml:"mode"`
	State    string `json:"state" yaml:"state"`
	// Cursor is the subscription's last acknowledged position. Lag is
	// how long the oldest event of the batch in hand has waited since uma
	// recorded it; empty once the plugin has caught up.
	Cursor       string `json:"cursor,omitempty" yaml:"cursor,omitempty"`
	Lag          string `json:"lag,omitempty" yaml:"lag,omitempty"`
	InFlight     int    `json:"in_flight" yaml:"in_flight"`
	Delivered    uint64 `json:"delivered" yaml:"delivered"`
	DeadLettered uint64 `json:"dead_lettered" yaml:"dead_lettered"`
	Skipped      uint64 `json:"skipped" yaml:"skipped"`
	LastEvent    string `json:"last_event,omitempty" yaml:"last_event,omitempty"`
	// LastError is the last failure: the event, when, and why.
	LastError *hooks.LastError `json:"last_error,omitempty" yaml:"last_error,omitempty"`
}

// hooksSection is the hook runner: empty when this run did not start
// it, an error when it failed, otherwise each plugin's cursor, lag and
// last error.
func hooksSection(all daemon.Status) cli.StatusSection {
	sec := runtimeSection(all, daemon.NameHooks, priorityHooks)
	rt, ok := sec.Data.(runtimeData)
	if !ok {
		return sec
	}
	data := hooksData{State: rt.State, Plugins: []hookData{}, NotReady: rt.NotReady}
	for _, h := range all.Hooks {
		d := hookData{
			Plugin: h.Plugin, Consumer: h.Consumer, Mode: h.Mode, State: string(h.State), Cursor: string(h.Cursor),
			InFlight: h.InFlight, Delivered: h.Delivered, DeadLettered: h.DeadLettered, Skipped: h.Skipped,
			LastEvent: h.LastEvent, LastError: h.LastError,
		}
		if h.Lag > 0 {
			d.Lag = h.Lag.Round(time.Second).String()
		}
		data.Plugins = append(data.Plugins, d)
	}
	sec.Data = data
	return sec
}

// runtimeData is a runtime service's section payload.
type runtimeData struct {
	State   daemon.ServiceState  `json:"state" yaml:"state"`
	Address string               `json:"address,omitempty" yaml:"address,omitempty"`
	Scopes  []daemon.ScopeStatus `json:"scopes" yaml:"scopes"`
	// NotReady lists the scopes that are not ready, as
	// <scope>=<state>[ (<reason>)], for the one-line table view.
	NotReady string `json:"not_ready,omitempty" yaml:"not_ready,omitempty"`
}

// scopeLabel names a scope for an operator: the channel's key when it
// has one, else the scope itself (channel:<id>, worker:2).
func scopeLabel(sc daemon.ScopeStatus) string {
	if sc.Key != "" {
		return sc.Key
	}
	return sc.Scope
}

func notReady(scopes []daemon.ScopeStatus) string {
	var out []string
	for _, sc := range scopes {
		if sc.State == daemon.ScopeReady {
			continue
		}
		s := fmt.Sprintf("%s=%s", scopeLabel(sc), sc.State)
		if sc.Reason != "" {
			s += " (" + sc.Reason + ")"
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// digestNames is how many not-ready scopes the one-line digest names.
const digestNames = 3

// notReadyDigest is the degraded section's one-line summary, for the
// table's DETAIL cell: how many scopes are not ready of all, by state,
// and the first few by name. A receiver that still receives while part
// of it is broken counts as "degraded"; its code is in the scope's json.
// It stays the same size at any scope count;
// the full list is the section's not_ready and scopes in json.
func notReadyDigest(scopes []daemon.ScopeStatus) string {
	var names []string
	counts := map[daemon.ScopeState]int{}
	for _, sc := range scopes {
		if sc.State == daemon.ScopeReady {
			continue
		}
		counts[sc.State]++
		names = append(names, scopeLabel(sc))
	}
	if len(names) == 0 {
		return "degraded"
	}
	var states []string
	for _, st := range []daemon.ScopeState{daemon.ScopeFailed, daemon.ScopeWaiting, daemon.ScopeStarting, daemon.ScopeDegraded} {
		if n := counts[st]; n > 0 {
			states = append(states, fmt.Sprintf("%s %d", st, n))
		}
	}
	shown := names
	if len(shown) > digestNames {
		shown = shown[:digestNames]
	}
	out := fmt.Sprintf("%d/%d not ready (%s): %s", len(names), len(scopes), strings.Join(states, ", "), strings.Join(shown, ", "))
	if more := len(names) - len(shown); more > 0 {
		out += fmt.Sprintf(" +%d more", more)
	}
	return out
}
