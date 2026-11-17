package matrix_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/composition"
	"hop.top/heo/internal/matrix"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// repoRoot is the repository root, from this package's directory.
const repoRoot = "../.."

func registry(t *testing.T) *adapters.Registry {
	t.Helper()
	r := adapters.NewRegistry()
	if err := composition.Register(r, composition.Options{Viper: func() *viper.Viper { return viper.New() }}); err != nil {
		t.Fatal(err)
	}
	return r
}

func rowsOf(t *testing.T) []matrix.Row {
	t.Helper()
	return matrix.Rows(registry(t))
}

// testNames is every Go test function in the repository's test files,
// with how many files declare each.
func testNames(t *testing.T) map[string]int {
	t.Helper()
	re := regexp.MustCompile(`(?m)^func (Test\w+)\(`)
	out := map[string]int{}
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "bin" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // G304: walking the repository's own test files
		if err != nil {
			return err
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			out[m[1]]++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRowsFollowTheDescriptors: one row for every maintained adapter and
// every provider-dependent verb, declared exactly when the adapter's
// descriptor enables the verb, with the descriptor's own maturity.
func TestRowsFollowTheDescriptors(t *testing.T) {
	r := registry(t)
	rows := matrix.Rows(r)
	verbs := adapters.ProviderVerbs()
	if want := r.Len() * len(verbs); len(rows) != want {
		t.Fatalf("%d rows, want %d (adapters x verbs)", len(rows), want)
	}
	i := 0
	for _, typ := range r.Types() {
		a, _ := r.Lookup(typ)
		d := a.Factory.Describe()
		enabled := adapters.VerbsOf(a)
		for _, v := range verbs {
			row := rows[i]
			i++
			if row.Adapter != typ || row.Verb != v.Name {
				t.Fatalf("row %d is %s / %s, want %s / %s", i, row.Adapter, row.Verb, typ, v.Name)
			}
			if row.Declared != slices.Contains(enabled, v.Name) {
				t.Errorf("%s / %s: declared = %v, descriptor enables %v", typ, v.Name, row.Declared, enabled)
			}
			if row.Maturity != d.Maturity || row.Maturity == "" {
				t.Errorf("%s / %s: maturity %q, descriptor says %q", typ, v.Name, row.Maturity, d.Maturity)
			}
		}
	}
}

// TestDeclaredVerbsPerAdapter pins the verbs each maintained adapter's
// declarations enable. A uma bump that changes them fails here until the
// matrix's evidence is reviewed with it; one that changes a declaration
// enabling no verb fails TestDeclaredCapabilitiesPerAdapter instead.
func TestDeclaredVerbsPerAdapter(t *testing.T) {
	all := []string{"message edit", "message delete", "reaction add", "reaction remove"}
	// state is the nine conversation state verbs a mailbox adapter's five
	// state capabilities enable; slack.chat and discord.chat declare
	// archive only.
	state := []string{"conversation archive", "conversation unarchive", "conversation move", "conversation read", "conversation unread",
		"conversation flag", "conversation unflag", "conversation trash", "conversation untrash"}
	want := map[model.TypeKey][]string{
		model.TypeNativeWebWidget: append(slices.Clone(all), "receipt send", "interaction respond"),
		model.TypeSmtpMail:        {},
		model.TypeImapMail:        slices.Concat([]string{"message raw", "history fetch"}, state),
		model.TypeGoogleMail:      slices.Concat([]string{"message raw", "history fetch", "conversation tag", "message reply-draft", "conversation unsubscribe"}, state),
		model.TypeMicrosoftMail:   slices.Concat([]string{"message raw", "history fetch", "conversation tag", "message reply-draft", "conversation unsubscribe"}, state),
		model.TypeTwilioSms:       {"history fetch"},
		model.TypeTwilioWhatsapp:  {},
		model.TypeMetaWhatsapp:    {"reaction add", "reaction remove"},
		model.TypeRelayMessages:   {},
		model.TypeGithubIssues:    append(slices.Clone(all), "history fetch", "conversation status", "conversation update"),
		model.TypeZendeskTickets:  {"conversation update"},
		model.TypeDiscordChat:     {"message edit", "message delete", "reaction add", "reaction remove", "participant add", "participant remove", "history fetch", "interaction respond", "conversation archive", "conversation unarchive"},
		model.TypeSlackChat: {"message edit", "message delete", "reaction add", "reaction remove", "history fetch", "interaction respond",
			"interaction open-modal", "interaction push-modal", "interaction update-modal", "conversation archive", "conversation unarchive"},
		model.TypeTelegramMessages: {"message edit", "message delete", "reaction add", "reaction remove", "interaction respond"},
		model.TypeMicrosoftTeams:   all,
		// gitlab.issues declares the same close and reopen as github.issues,
		// so conversation status is enabled on it; none of the three
		// ships an update effect.
		model.TypeGitlabIssues: {"message edit", "message delete", "history fetch", "conversation status"},
		model.TypeJiraIssues:   {"message edit", "message delete"},
		model.TypeLinearIssues: {"message edit", "message delete"},
	}
	got := map[model.TypeKey][]string{}
	for _, row := range rowsOf(t) {
		if _, ok := got[row.Adapter]; !ok {
			got[row.Adapter] = []string{}
		}
		if row.Declared {
			got[row.Adapter] = append(got[row.Adapter], row.Verb)
		}
	}
	if len(got) != len(want) {
		t.Errorf("adapters = %d, pinned %d", len(got), len(want))
	}
	for typ, w := range want {
		if !slices.Equal(got[typ], w) {
			t.Errorf("%s declares %v, pinned %v", typ, got[typ], w)
		}
	}
}

// declaredEffects is how uma's adapter packages declare their extension
// effects and reads: an Extensions method on the factory, which heo
// registers beside it or not (Adapter.Extensions).
type declaredEffects interface {
	Extensions() []adapter.ExtensionDefinition
}

// pinDiff is what got adds to and drops from want, as sets.
func pinDiff(got, want []string) (added, removed []string) {
	for _, g := range got {
		if !slices.Contains(want, g) {
			added = append(added, g)
		}
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			removed = append(removed, w)
		}
	}
	return added, removed
}

// TestDeclaredCapabilitiesPerAdapter pins what each maintained adapter
// declares to uma, whether or not a heo verb uses it: its capabilities,
// and its extension effects and reads, each with the capability it
// requires. TestDeclaredVerbsPerAdapter only sees a change that alters
// which verbs are enabled; a new capability, or a new effect of a
// capability already declared, enables none and would pass there. A uma
// bump changing a declaration fails here until it is reviewed (verbs,
// matrix evidence, credential purposes, docs) and the pin updated.
func TestDeclaredCapabilitiesPerAdapter(t *testing.T) {
	type pin struct {
		capabilities []string
		// effects are "<name> v<version> <kind> needs <capability>".
		effects []string
	}
	want := map[model.TypeKey]pin{
		model.TypeNativeWebWidget: {capabilities: []string{"receive_http", "send", "start_conversation", "stream", "send_ephemeral",
			"signal", "edit", "delete", "react", "receipt", "respond", "respond_follow_up"}},
		// reply_recipients (reply-all, cc and bcc on a reply) is declared by
		// the four mail adapters; heo's `message reply` and `message
		// reply-draft` take --reply-all, --cc and --bcc where it is, not
		// as verbs of their own: a reply without them needs no capability.
		model.TypeSmtpMail: {capabilities: []string{"receive", "send", "start_conversation", "reply_recipients"}},
		// archive, move, read_state, flag and trash (and slack's and
		// discord's archive) are declared by the adapters' conversation
		// state support; heo's conversation archive, unarchive, move, read,
		// unread, flag, unflag, trash and untrash serve them, and
		// TestDeclaredVerbsPerAdapter pins which verbs each enables.
		model.TypeImapMail: {
			capabilities: []string{"receive", "send", "reconcile", "start_conversation",
				"archive", "move", "read_state", "flag", "trash", "history", "reply_recipients",
				"raw_read", "extension_query", "extension_execute", "imap.mail.mailboxes", "imap.mail.mailbox_create"},
			effects: []string{"imap.mail.mailboxes v1 read needs imap.mail.mailboxes",
				"imap.mail.mailbox_create v1 effect needs imap.mail.mailbox_create"},
		},
		model.TypeGoogleMail: {
			capabilities: []string{"receive", "receive_http", "send", "reconcile", "start_conversation", "extension_execute",
				"archive", "move", "read_state", "flag", "trash", "history", "reply_recipients",
				"raw_read", "extension_query", "google.mail.mailboxes", "google.mail.mailbox_create"},
			effects: []string{"google.mail.mailboxes v1 read needs google.mail.mailboxes",
				"google.mail.mailbox_create v1 effect needs google.mail.mailbox_create", "google.mail.labels v1 effect needs extension_execute",
				"google.mail.reply_draft v1 effect needs extension_execute", "google.mail.unsubscribe v1 effect needs extension_execute"},
		},
		model.TypeMicrosoftMail: {
			capabilities: []string{"receive", "receive_http", "send", "reconcile", "start_conversation", "extension_execute",
				"archive", "move", "read_state", "flag", "trash", "history", "reply_recipients",
				"raw_read", "extension_query", "microsoft.mail.mailboxes", "microsoft.mail.mailbox_create"},
			effects: []string{"microsoft.mail.mailboxes v1 read needs microsoft.mail.mailboxes",
				"microsoft.mail.mailbox_create v1 effect needs microsoft.mail.mailbox_create", "microsoft.mail.categories v1 effect needs extension_execute",
				"microsoft.mail.reply_draft v1 effect needs extension_execute", "microsoft.mail.unsubscribe v1 effect needs extension_execute"},
		},
		model.TypeTwilioSms:      {capabilities: []string{"send", "reconcile", "receive_http", "start_conversation", "history"}},
		model.TypeTwilioWhatsapp: {capabilities: []string{"send", "reconcile", "receive_http", "start_conversation"}},
		model.TypeMetaWhatsapp:   {capabilities: []string{"receive_http", "send", "react", "start_conversation"}},
		model.TypeRelayMessages:  {capabilities: []string{"receive_http", "send", "reconcile", "start_conversation"}},
		model.TypeDiscordChat: {capabilities: []string{"receive", "receive_http", "send", "start_conversation", "edit", "delete", "react",
			"membership", "history", "reconcile", "refetch", "send_ephemeral", "signal", "respond", "respond_follow_up", "archive"}},
		model.TypeSlackChat: {
			capabilities: []string{"receive", "receive_http", "send", "start_conversation", "edit", "delete", "react",
				"reconcile", "history", "respond", "respond_follow_up", "extension_execute", "archive"},
			effects: []string{
				"slack.chat.views.open v1 effect needs extension_execute",
				"slack.chat.views.update v1 effect needs extension_execute",
				"slack.chat.views.push v1 effect needs extension_execute",
			},
		},
		model.TypeTelegramMessages: {capabilities: []string{"receive", "receive_http", "send", "start_conversation", "edit", "delete",
			"react", "respond"}},
		model.TypeMicrosoftTeams: {capabilities: []string{"receive_http", "send", "start_conversation", "edit", "delete", "react", "signal"}},
		model.TypeGithubIssues: {
			capabilities: []string{"receive_http", "send", "reconcile", "refetch", "work_item_act", "edit", "delete", "react", "history",
				"extension_execute"},
			effects: []string{"github.issues.update v1 effect needs extension_execute"},
		},
		model.TypeZendeskTickets: {
			capabilities: []string{"receive", "receive_http", "send", "reconcile", "extension_execute", "internal_note"},
			effects: []string{
				"zendesk.tickets.internal_note v1 effect needs extension_execute",
				"zendesk.tickets.update v1 effect needs extension_execute",
			},
		},
		model.TypeGitlabIssues: {capabilities: []string{"receive_http", "send", "reconcile", "refetch", "work_item_act", "edit", "delete", "history"}},
		model.TypeJiraIssues:   {capabilities: []string{"receive_http", "send", "edit", "delete", "reconcile", "refetch"}},
		model.TypeLinearIssues: {capabilities: []string{"receive_http", "send", "reconcile", "refetch", "edit", "delete"}},
	}
	r := registry(t)
	seen := map[model.TypeKey]bool{}
	for _, typ := range r.Types() {
		seen[typ] = true
		w, pinned := want[typ]
		if !pinned {
			t.Errorf("%s is registered and not pinned: add its capabilities and effects to TestDeclaredCapabilitiesPerAdapter", typ)
			continue
		}
		a, _ := r.Lookup(typ)
		var caps []string
		for _, c := range a.Factory.Describe().Capabilities {
			caps = append(caps, string(c))
		}
		var effects []string
		if x, ok := a.Factory.(declaredEffects); ok {
			for _, e := range x.Extensions() {
				effects = append(effects, fmt.Sprintf("%s v%d %s needs %s", e.Name, e.Version, e.Kind, e.RequiredCapability))
			}
		}
		for _, p := range []struct {
			what      string
			got, want []string
		}{{"capabilities", caps, w.capabilities}, {"extension effects", effects, w.effects}} {
			if added, removed := pinDiff(p.got, p.want); len(added)+len(removed) > 0 {
				t.Errorf("%s declares different %s than pinned: added %q, removed %q; review the verbs, matrix evidence and docs "+
					"it touches, then update the pin", typ, p.what, added, removed)
			}
		}
	}
	for typ := range want {
		if !seen[typ] {
			t.Errorf("%s is pinned and no longer registered: drop its pin", typ)
		}
	}
}

// TestEveryDeclaredVerbHasEvidence: a declared verb must have an
// evidence entry, even when it says none, so a new capability cannot join
// the table unexamined; and evidence names only verbs the adapter
// declares.
func TestEveryDeclaredVerbHasEvidence(t *testing.T) {
	declared := map[string]bool{}
	for _, row := range rowsOf(t) {
		if !row.Declared {
			continue
		}
		key := string(row.Adapter) + " / " + row.Verb
		declared[key] = true
		if !row.HasEvidence {
			t.Errorf("%s is declared and has no evidence entry; add one, with Against none if nothing tests it", key)
		}
	}
	for _, k := range matrix.EvidenceKeys() {
		if !declared[k] {
			t.Errorf("evidence for %s, which no adapter declares", k)
		}
	}
}

// TestEvidenceNamesRealTests: every test a row, a gate or a recording
// status cites exists, once, in the repository's test files.
func TestEvidenceNamesRealTests(t *testing.T) {
	have := testNames(t)
	cited := matrix.CitedTests(registry(t))
	if len(cited) == 0 {
		t.Fatal("the matrix cites no test")
	}
	for _, name := range cited {
		switch n := have[name]; {
		case n == 0:
			t.Errorf("the matrix cites %s, which no test file declares", name)
		case n > 1:
			t.Errorf("the matrix cites %s, which %d test files declare: the name must be unique", name, n)
		}
	}
}

// TestRowsClaimNoMoreThanTheirTests: what a row says about where it was
// recorded must be what its tests are.
func TestRowsClaimNoMoreThanTheirTests(t *testing.T) {
	for _, row := range rowsOf(t) {
		key := string(row.Adapter) + " / " + row.Verb
		behavior := 0
		for _, tc := range row.Tests {
			if tc.Behavior {
				behavior++
			}
		}
		switch row.Against {
		case matrix.None:
			if behavior != 0 {
				t.Errorf("%s: recorded against none, yet cites a behavior test", key)
			}
		case matrix.RealSandbox, matrix.RealLiveAccount, matrix.Fake, matrix.InProcess:
			if behavior == 0 {
				t.Errorf("%s: recorded against %s, yet no behavior test backs it", key, row.Against)
			}
		default:
			t.Errorf("%s: unknown recorded-against %q", key, row.Against)
		}
		if !row.Declared && row.Against != matrix.Fake {
			t.Errorf("%s: an undeclared verb's row is the refusal, shown on the fake provider, not %s", key, row.Against)
		}
		if row.Against == matrix.RealSandbox || row.Against == matrix.RealLiveAccount {
			var backed bool
			for _, tc := range row.Tests {
				if tc.Behavior && cassettes(t, tc.Name) > 0 {
					backed = true
				}
			}
			if !backed {
				t.Errorf("%s: recorded against a real private sandbox, yet none of its tests has committed cassettes", key)
			}
		}
		if !row.Declared && len(row.Tests) == 0 {
			t.Errorf("%s: an undeclared verb cites no test of the refusal", key)
		}
		if row.Declared && len(row.Tests) == 0 && row.Against != matrix.None {
			t.Errorf("%s: cites no test", key)
		}
	}
}

// TestStatusesClaimNoMoreThanTheirTests: what an adapter's recording
// status says it was recorded against is what its tests are: a real
// account or sandbox needs a behavior test with committed cassettes, and
// none needs no behavior test at all. Every adapter has a status.
func TestStatusesClaimNoMoreThanTheirTests(t *testing.T) {
	r := registry(t)
	for _, typ := range r.Types() {
		st, ok := matrix.StatusOf(typ)
		if !ok {
			t.Errorf("%s has no recording status", typ)
			continue
		}
		behavior, recorded := 0, false
		for _, tc := range st.Tests {
			if tc.Behavior {
				behavior++
				recorded = recorded || cassettes(t, tc.Name) > 0
			}
		}
		switch st.Against {
		case matrix.None:
			if behavior != 0 {
				t.Errorf("%s: recording status none, yet cites a behavior test", typ)
			}
		case matrix.RealSandbox, matrix.RealLiveAccount:
			if !recorded {
				t.Errorf("%s: recorded against %s, yet none of its behavior tests has committed cassettes", typ, st.Against)
			}
		case matrix.Fake, matrix.InProcess:
			if behavior == 0 {
				t.Errorf("%s: recorded against %s, yet no behavior test backs it", typ, st.Against)
			}
		default:
			t.Errorf("%s: unknown recorded-against %q", typ, st.Against)
		}
	}
}

// cassettes counts the committed requests of test name.
func cassettes(t *testing.T, name string) int {
	t.Helper()
	n := 0
	root := filepath.Join(repoRoot, "tests", "e2e", "testdata", "cassettes", name)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".req.yaml") {
			n++
		}
		return nil
	})
	return n
}

// TestRenderedMatrix: the document block has a line for every row, says
// for each only what its evidence says, cites no test the evidence does
// not, and fits the table width the docs keep.
func TestRenderedMatrix(t *testing.T) {
	r := registry(t)
	rows := matrix.Rows(r)
	out := matrix.Render(r)
	cited := map[string]bool{}
	for _, n := range matrix.CitedTests(r) {
		cited[n] = true
	}
	for _, m := range regexp.MustCompile("`(Test\\w+)`").FindAllStringSubmatch(out, -1) {
		if !cited[m[1]] {
			t.Errorf("the rendered matrix cites %s, which the evidence does not", m[1])
		}
	}
	for _, row := range rows {
		line := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "| `"+string(row.Adapter)+"` | "+row.Verb+" |") {
				line = l
			}
		}
		if line == "" {
			t.Errorf("no row for %s / %s", row.Adapter, row.Verb)
			continue
		}
		if !strings.Contains(line, string(row.Against)) || !strings.Contains(line, row.Maturity) {
			t.Errorf("row for %s / %s lacks its recorded-against or maturity: %s", row.Adapter, row.Verb, line)
		}
		for _, tc := range row.Tests {
			if !strings.Contains(line, "`"+tc.Name+"`") {
				t.Errorf("row for %s / %s lacks %s: %s", row.Adapter, row.Verb, tc.Name, line)
			}
		}
		if len(row.Tests) == 0 && !strings.Contains(line, "| none |") {
			t.Errorf("row for %s / %s cites no test and does not say none: %s", row.Adapter, row.Verb, line)
		}
	}
	for i, l := range strings.Split(out, "\n") {
		limit := 100
		if strings.HasPrefix(l, "|") {
			limit = 256
		}
		if n := len([]rune(l)); n > limit {
			t.Errorf("rendered line %d: %d columns, limit %d", i+1, n, limit)
		}
	}
}

// TestSpliceReplacesOnlyTheMarkedBlock.
func TestSpliceReplacesOnlyTheMarkedBlock(t *testing.T) {
	doc := "intro\n" + matrix.Begin + "\nold\n" + matrix.End + "\noutro\n"
	got, err := matrix.Splice(doc, "new\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := "intro\n" + matrix.Begin + "\nnew\n" + matrix.End + "\noutro\n"; got != want {
		t.Errorf("splice = %q, want %q", got, want)
	}
	for _, bad := range []string{"no markers", matrix.End + "\n" + matrix.Begin, matrix.Begin + "\nonly"} {
		if _, err := matrix.Splice(bad, "x"); err == nil {
			t.Errorf("splice accepted %q", bad)
		}
	}
}
