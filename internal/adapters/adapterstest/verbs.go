package adapterstest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// The conversation verbs the fake can declare, each opt-in: membership
// (participant add and remove), receipt (receipt send) and history
// (history fetch). A fake that declares none of them behaves as before,
// so a test holds one adapter that declares and one that does not.

// HistoryRoute is the webhook path, relative to the channel's route,
// where a test hands the channel's provider the history it holds, as
// the account's past messages would be: POST a HistoryHold. Nothing
// reaches the sink; the held envelopes are served by History, to an
// import that asks.
const HistoryRoute = "/history"

// HistoryHold is the provider history a test sets on a channel.
type HistoryHold struct {
	// Items are the past messages, as an Inject would deliver them. An
	// item without OccurredAt is held as of now.
	Items []Inject `json:"items"`
	// Horizon, when set, is how far back the provider's window reaches:
	// it holds nothing older, and a window that begins before it is
	// served with a gap marked, as a provider past its retention
	// would. Zero means the window is whole.
	Horizon time.Time `json:"horizon,omitzero"`
}

// held is a channel's held history.
type held struct {
	items   []Inject
	horizon time.Time
}

// DeclareMembership makes f declare membership and apply each change:
// the provider accepts it and records it (see Effects, LogEffects).
// Call it before the engine starts.
func (f *Factory) DeclareMembership() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members = true
}

// DeclareReceipts makes f declare receipt and send each receipt: the
// provider accepts it and records it. Call it before the engine starts.
func (f *Factory) DeclareReceipts() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receipts = true
}

// DeclareHistory makes f declare history and serve what HoldHistory
// set; with LogEffects set, each read appends "history since=<time>
// cursor=<cursor> limit=<n>" to the file. Call it before the engine
// starts.
func (f *Factory) DeclareHistory() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history = true
}

// HoldHistory sets the provider history of channel ch: the past
// messages its account holds, and how far back the provider's window
// reaches (zero for the whole). It replaces what was held.
func (f *Factory) HoldHistory(ch model.ID, h HistoryHold) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[ch] = holdOf(h, time.Now().UTC())
}

// holdOf stamps the items without a time at now.
func holdOf(h HistoryHold, now time.Time) *held {
	out := &held{items: slices.Clone(h.Items), horizon: h.Horizon.UTC()}
	for i := range out.items {
		if out.items[i].OccurredAt.IsZero() {
			out.items[i].OccurredAt = now
		}
	}
	return out
}

// HistoryReads returns the history requests f answered, in order.
func (f *Factory) HistoryReads() []adapter.HistoryRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.historyReads)
}

// hold takes the HistoryHold r carries.
func (i *instance) hold(w http.ResponseWriter, r *http.Request) {
	var h HistoryHold
	if err := json.NewDecoder(r.Body).Decode(&h); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	i.f.HoldHistory(i.ch.Ref.ID, h)
	w.WriteHeader(http.StatusNoContent)
}

// History serves a page of the channel's held history, oldest first:
// the items at or after Since and after the horizon, those of the
// conversation on Route when it is set. The cursor is a position in
// that window. A window that begins before the horizon is marked a gap
// on its first page: the provider could not return the part before it.
func (i *instance) History(ctx context.Context, r adapter.HistoryRequest) (adapter.HistoryPage, error) {
	i.f.mu.Lock()
	defer i.f.mu.Unlock()
	i.f.historyReads = append(i.f.historyReads, r)
	appendLine(i.f.effectFile, fmt.Sprintf("history since=%s cursor=%s limit=%d", r.Since.UTC().Format(time.RFC3339), r.Cursor, r.Limit))
	h := i.f.held[i.ch.Ref.ID]
	if h == nil {
		h = &held{}
	}
	items := slices.Clone(h.items)
	slices.SortStableFunc(items, func(a, b Inject) int { return a.OccurredAt.Compare(b.OccurredAt) })
	var window []model.Envelope
	for _, in := range items {
		if in.OccurredAt.Before(r.Since) || in.OccurredAt.Before(h.horizon) {
			continue
		}
		env, err := i.envelope(ctx, nil, in)
		if err != nil {
			return adapter.HistoryPage{}, err
		}
		if r.Route != nil && env.Sender != *r.Route && !slices.Contains(env.References, *r.Route) {
			continue
		}
		window = append(window, env)
	}
	start := 0
	if r.Cursor != "" {
		n, err := strconv.Atoi(r.Cursor)
		if err != nil || n < 0 || n > len(window) {
			return adapter.HistoryPage{}, model.NewError(model.Validation, "fake provider: unknown history cursor")
		}
		start = n
	}
	end := min(start+max(r.Limit, 1), len(window))
	page := adapter.HistoryPage{Envelopes: window[start:end], Gap: r.Cursor == "" && r.Since.Before(h.horizon)}
	if end < len(window) {
		page.Next = strconv.Itoa(end)
	}
	return page, nil
}

// SendReceipt sends a receipt, after DeclareReceipts: it records it as
// "receipt <target> <kind>", the target being the provider message ID.
func (i *instance) SendReceipt(_ context.Context, r adapter.ReceiptRequest) (adapter.Outcome, error) {
	return i.effect(Effect{Kind: "receipt", Receipt: r.Kind}, r.Target)
}

// ChangeMembership adds a participant or removes it, after
// DeclareMembership: it records it as "member add <participant>" or
// "member remove <participant>", the participant being the identity's
// provider ID.
func (i *instance) ChangeMembership(_ context.Context, r adapter.MembershipRequest) (adapter.Outcome, error) {
	return i.effect(Effect{Kind: "member", Participant: r.Participant, Remove: r.Remove}, nil)
}

// effectLine is the line an applied effect appends to the effects log.
func effectLine(e Effect) string {
	line := e.Kind + " " + e.Target.ID
	switch {
	case e.Kind == "edit":
		line += " " + e.Text
	case e.Kind == "react" && e.Remove:
		line = "unreact " + e.Target.ID + " " + e.Emoji
	case e.Kind == "react":
		line += " " + e.Emoji
	case e.Kind == "receipt":
		line += " " + e.Receipt
	case e.Kind == "state":
		line = "state " + e.State.Verb + " " + e.Target.ID + e.State.detail()
	case e.Kind == "member":
		verb := "add"
		if e.Remove {
			verb = "remove"
		}
		line = fmt.Sprintf("member %s %s", verb, e.Participant.ID)
	}
	return line
}

// appendLine appends line to the file at path, when set.
func appendLine(path, line string) {
	if path == "" {
		return
	}
	if fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil { //nolint:gosec // test-chosen path
		_, _ = fmt.Fprintln(fh, line)
		_ = fh.Close()
	}
}
