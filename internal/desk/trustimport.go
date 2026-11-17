package desk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"hop.top/heo/internal/trustcfg"
	"hop.top/uma/model"
)

// TrustImportVersion is the only version of the import file.
const TrustImportVersion = 1

// TrustImportFile is the file `operation trust import` takes: the trust
// a deployment earned elsewhere, one entry per trust key. It holds no
// message data, subject or address.
type TrustImportFile struct {
	Version int                `json:"version"`
	Entries []TrustImportEntry `json:"entries"`
}

// TrustImportEntry is the earned trust of one key. Its five key parts
// are the ones TrustKey builds a key from. Clean is required, so an
// absent count is told from a zero one.
type TrustImportEntry struct {
	// Channel is the inbox the writes touched, by key or record ID.
	Channel    string `json:"channel"`
	Proposer   string `json:"proposer"`
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
	Verb       string `json:"verb"`
	Class      string `json:"class,omitempty"`
	// Clean is the consecutive clean approvals since the last correction.
	Clean *int64 `json:"clean"`
	// LastCorrectionAt is the time of the latest correction.
	LastCorrectionAt *time.Time `json:"last_correction_at,omitempty"`
}

// ParseTrustImport reads an import file strictly: a field outside the
// schema, a second JSON value or a version other than 1 is refused
// (model.Validation) and nothing is read of it.
func ParseTrustImport(b []byte) (TrustImportFile, error) {
	var f TrustImportFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return TrustImportFile{}, model.NewError(model.Validation, "trust import file: "+err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return TrustImportFile{}, model.NewError(model.Validation, "trust import file: more than one JSON value")
	}
	if f.Version != TrustImportVersion {
		return TrustImportFile{}, model.NewError(model.Validation, fmt.Sprintf(
			"trust import file: version %d is not supported, want %d", f.Version, TrustImportVersion))
	}
	if len(f.Entries) == 0 {
		return TrustImportFile{}, model.NewError(model.Validation, "trust import file: entries: none to import")
	}
	return f, nil
}

// trustVerbClasses lists the verbs a write holds under, with the target
// classes each takes: the hold points' own vocabulary. A nil list takes
// any non-empty class, and an empty one takes none.
var trustVerbClasses = map[string][]string{
	"reply":   {"send"},
	"start":   {},
	"note":    {string(PlacementLocal), string(PlacementProvider)},
	"state":   nil,
	"move":    nil,
	"label":   nil,
	"unlabel": nil,
	"tag":     nil,
	"edit":    {"message"},
	"delete":  {"message"},
	"react":   {"message"},
}

// TrustImportVerbs lists the verbs an entry may name.
func TrustImportVerbs() []string { return slices.Sorted(maps.Keys(trustVerbClasses)) }

// checkTrustVerb refuses a verb and class no write is held under.
func checkTrustVerb(verb, class string) string {
	if verb == "label-create" {
		return "verb label-create: its key names no mailbox and the ladder never reads its counters, so there is nothing to import"
	}
	if verb == trustcfg.UnsubscribeClass {
		return "verb unsubscribe: irreversible, always held whatever the count, so there is nothing to import"
	}
	classes, ok := trustVerbClasses[verb]
	if !ok {
		return fmt.Sprintf("verb %q: not a held verb; want one of %s", verb, strings.Join(TrustImportVerbs(), ", "))
	}
	switch {
	case classes == nil && class == "":
		return fmt.Sprintf("class: verb %s needs one (its target: a state, a label or tag name)", verb)
	case classes != nil && len(classes) == 0 && class != "":
		return fmt.Sprintf("class %q: verb %s takes none", class, verb)
	case len(classes) > 0 && !slices.Contains(classes, class):
		return fmt.Sprintf("class %q: verb %s takes %s", class, verb, strings.Join(classes, " or "))
	}
	return ""
}

// Statuses of an entry in a trust import result.
const (
	TrustImported   = "imported"
	TrustUnchanged  = "unchanged"
	TrustRefused    = "refused"
	TrustFailed     = "failed"
	TrustNotWritten = "not_written"
)

// TrustImportRow is what became of one entry.
type TrustImportRow struct {
	// Index is the entry's position in the file, from 0.
	Index  int    `json:"index"`
	Key    string `json:"key,omitempty"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	// Clean, Approvals, Corrections and LastCorrectionAt are the key's
	// counters once imported (or as an import would leave them).
	Clean            int64      `json:"clean"`
	Approvals        int64      `json:"approvals"`
	Corrections      int64      `json:"corrections"`
	LastCorrectionAt *time.Time `json:"last_correction_at,omitempty"`
	// Automatic says whether the trust ladder, under the current
	// configuration, runs the key without a decision; BecomesAutomatic,
	// that the import is what makes it so. HoldReason says why not.
	Automatic        bool   `json:"automatic"`
	BecomesAutomatic bool   `json:"becomes_automatic"`
	HoldReason       string `json:"hold_reason,omitempty"`
}

// TrustImportResult is the outcome of an import. Counts add up to the
// entries: imported and unchanged are the entries written or already
// as the file has them (under DryRun, as they would be), refused the
// entries that fail validation, failed those a write failed on. One
// refused entry refuses the file: nothing is written, and the valid
// ones are not_written.
type TrustImportResult struct {
	DryRun    bool             `json:"dry_run"`
	Imported  int              `json:"imported"`
	Unchanged int              `json:"unchanged"`
	Refused   int              `json:"refused"`
	Failed    int              `json:"failed"`
	Automatic int              `json:"becomes_automatic"`
	Entries   []TrustImportRow `json:"entries"`
}

// OK reports whether every entry was imported or was already so.
func (r TrustImportResult) OK() bool { return r.Refused == 0 && r.Failed == 0 }

// TrustImportOptions is how an import runs.
type TrustImportOptions struct {
	// DryRun writes nothing: it reports what an import would do.
	DryRun bool
	// Config decides, per key, whether the ladder runs it.
	Config trustcfg.Config
	// Now is the clock the supervised week reads.
	Now time.Time
	// Read reads the stored counters as the runtime does: the caller
	// holds no right to read trust, only to import it.
	Read TrustLookup
}

// TrustLookup is the part of uma's operations an import reads.
type TrustLookup interface {
	Trust(ctx context.Context, req model.TrustLookup) (model.TrustEntry, error)
}

type trustPlan struct {
	row TrustImportRow
	imp model.ImportTrust
}

// ImportTrust seeds the counters of the file's keys through uma's
// import, as the caller (which must hold trust.write). Every entry is
// validated first, its key built with TrustKey; one refusal refuses the
// file and nothing is written. uma merges, never replaces, so a repeat
// leaves everything as it is. uma writes key by key: a write that fails
// part way leaves the earlier keys imported, reported as imported, and
// the rest not_written.
func (s Service) ImportTrust(ctx context.Context, f TrustImportFile, o TrustImportOptions) (TrustImportResult, error) {
	plans, err := s.planTrustImport(ctx, f)
	if err != nil {
		return TrustImportResult{}, err
	}
	res := TrustImportResult{DryRun: o.DryRun, Entries: make([]TrustImportRow, len(plans))}
	for _, p := range plans {
		if p.row.Status == TrustRefused {
			res.Refused++
		}
	}
	writing := res.Refused == 0 && !o.DryRun
	stopped := false
	for i, p := range plans {
		row := p.row
		switch {
		case row.Status == TrustRefused:
		case res.Refused > 0 || stopped:
			row.Status = TrustNotWritten
		default:
			var err error
			if row, err = s.importTrustEntry(ctx, p, o, writing); err != nil {
				row = TrustImportRow{Index: p.row.Index, Key: p.imp.Key, Status: TrustFailed, Reason: err.Error()}
				stopped = true
			}
		}
		switch row.Status {
		case TrustImported:
			res.Imported++
		case TrustUnchanged:
			res.Unchanged++
		case TrustFailed:
			res.Failed++
		}
		if row.BecomesAutomatic {
			res.Automatic++
		}
		res.Entries[i] = row
	}
	return res, nil
}

// planTrustImport validates every entry and builds its key. A refused
// entry carries its reason in its row.
func (s Service) planTrustImport(ctx context.Context, f TrustImportFile) ([]trustPlan, error) {
	inboxes := map[string]*model.Inbox{}
	seen := map[string]int{}
	plans := make([]trustPlan, len(f.Entries))
	for i, e := range f.Entries {
		p := trustPlan{row: TrustImportRow{Index: i, Status: TrustRefused}}
		in, reason, err := s.trustEntryInbox(ctx, e.Channel, inboxes)
		if err != nil {
			return nil, err
		}
		if reason == "" {
			reason = checkTrustEntry(e)
		}
		if reason == "" {
			p.imp = model.ImportTrust{Call: call(), Channel: in.Channel, Clean: *e.Clean, LastCorrectionAt: e.LastCorrectionAt}
			p.imp.Key, p.imp.Labels = TrustKey(model.ActorID(e.Proposer), model.ActorID(e.OnBehalfOf), string(in.Ref.ID), e.Verb, e.Class)
			p.row.Key = p.imp.Key
			if err := p.imp.Validate(); err != nil {
				reason = strings.TrimPrefix(err.Error(), "validation: ")
			} else if j, dup := seen[p.imp.Key]; dup {
				reason = fmt.Sprintf("same key as entry %d: list each key once", j)
			}
		}
		if reason == "" {
			p.row.Status = ""
			seen[p.imp.Key] = i
		} else {
			p.row.Reason = fmt.Sprintf("entry %d: %s", i, reason)
		}
		plans[i] = p
	}
	return plans, nil
}

// trustEntryInbox resolves the inbox an entry names. An unknown one is
// a refusal; any other failure fails the import.
func (s Service) trustEntryInbox(ctx context.Context, ref string, seen map[string]*model.Inbox) (model.Inbox, string, error) {
	if ref == "" {
		return model.Inbox{}, "channel: required", nil
	}
	if in, ok := seen[ref]; ok {
		if in == nil {
			return model.Inbox{}, fmt.Sprintf("channel %q: no inbox has this key or ID", ref), nil
		}
		return *in, "", nil
	}
	in, err := s.FindInbox(ctx, ref)
	switch {
	case model.IsCode(err, model.NotFound):
		seen[ref] = nil
		return model.Inbox{}, fmt.Sprintf("channel %q: no inbox has this key or ID", ref), nil
	case err != nil:
		return model.Inbox{}, "", err
	}
	seen[ref] = &in
	return in, "", nil
}

// checkTrustEntry validates what the entry says by itself.
func checkTrustEntry(e TrustImportEntry) string {
	switch {
	case e.Proposer == "":
		return "proposer: required"
	case !model.ActorID(e.Proposer).IsPlugin():
		return fmt.Sprintf("proposer %q: only a plugin: actor is held, so only it earns trust", e.Proposer)
	case e.Verb == "":
		return "verb: required"
	case e.Clean == nil:
		return "clean: required"
	case *e.Clean < 0:
		return "clean: must not be negative"
	}
	return checkTrustVerb(e.Verb, e.Class)
}

// importTrustEntry reads the key's counters, merges the entry in (uma's
// import when write, a projection of it when not) and decides the key.
func (s Service) importTrustEntry(ctx context.Context, p trustPlan, o TrustImportOptions, write bool) (TrustImportRow, error) {
	var before *model.TrustEntry
	switch e, err := o.Read.Trust(ctx, model.TrustLookup{Call: call(), Channel: p.imp.Channel, Key: p.imp.Key}); {
	case err == nil:
		before = &e
	case !model.IsCode(err, model.NotFound):
		return TrustImportRow{}, err
	}
	var after model.TrustEntry
	if write {
		var err error
		if after, err = s.Client.Operations().ImportTrust(ctx, p.imp); err != nil {
			return TrustImportRow{}, err
		}
	} else {
		after = MergeTrust(before, p.imp)
	}
	row := p.row
	row.Status = TrustImported
	if before != nil && sameTrust(*before, after) {
		row.Status = TrustUnchanged
	}
	row.Clean, row.Approvals, row.Corrections, row.LastCorrectionAt = after.Clean, after.Approvals, after.Corrections, after.LastCorrectionAt
	was := false
	if before != nil {
		was = TrustRowOf(o.Config, *before, o.Now).Automatic
	}
	now := TrustRowOf(o.Config, after, o.Now)
	row.Automatic, row.HoldReason = now.Automatic, now.Reason
	row.BecomesAutomatic = now.Automatic && !was
	return row, nil
}

func sameTrust(a, b model.TrustEntry) bool {
	same := a.LastCorrectionAt == nil && b.LastCorrectionAt == nil ||
		a.LastCorrectionAt != nil && b.LastCorrectionAt != nil && a.LastCorrectionAt.Equal(*b.LastCorrectionAt)
	return same && a.Clean == b.Clean && a.Approvals == b.Approvals && a.Corrections == b.Corrections && maps.Equal(a.Labels, b.Labels)
}

// MergeTrust is what uma's import leaves of a stored entry (nil when
// the key was never counted) and imp: each total the larger of the two,
// the last correction the later, and the consecutive count raised only
// while no stored correction is later than the imported history's. It
// mirrors uma's merge so a dry run can show the result without writing;
// a test holds it to uma's own answers.
func MergeTrust(stored *model.TrustEntry, imp model.ImportTrust) model.TrustEntry {
	approvals := max(imp.Approvals, imp.Clean)
	corrections := imp.Corrections
	if corrections == 0 && imp.LastCorrectionAt != nil {
		corrections = 1
	}
	var out model.TrustEntry
	if stored != nil {
		out = *stored
		out.Labels = maps.Clone(stored.Labels)
	}
	out.Key = imp.Key
	if stored == nil || stored.LastCorrectionAt == nil || imp.LastCorrectionAt != nil && !stored.LastCorrectionAt.After(*imp.LastCorrectionAt) {
		out.Clean = max(out.Clean, imp.Clean)
	}
	out.Approvals = max(out.Approvals, approvals)
	out.Corrections = max(out.Corrections, corrections)
	if imp.LastCorrectionAt != nil && (out.LastCorrectionAt == nil || imp.LastCorrectionAt.After(*out.LastCorrectionAt)) {
		t := imp.LastCorrectionAt.UTC()
		out.LastCorrectionAt = &t
	}
	if imp.Labels != nil && !maps.Equal(imp.Labels, out.Labels) {
		out.Labels = maps.Clone(imp.Labels)
	}
	return out
}
