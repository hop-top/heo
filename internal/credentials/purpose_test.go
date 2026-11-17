package credentials

import (
	"context"
	"slices"
	"strings"
	"testing"

	"hop.top/heo/internal/profile"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// keyringBackend is a native profile on a stubbed OS keyring: a flat
// store holding a reference's shared entry next to its purpose
// entries.
func keyringBackend(t *testing.T) *Backend {
	t.Helper()
	stubKeyring(t)
	return open(t, native, Config{Backend: BackendKeyring, KeyringService: "heo-test"})
}

// storePurpose writes value under the store key of ref's own entry for
// purpose.
func storePurpose(t *testing.T, b *Backend, ref, purpose string, value []byte) {
	t.Helper()
	keys, err := b.Keys(ref, purpose)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("Keys(%q, %q) = %v, want a purpose key and the shared key", ref, purpose, keys)
	}
	if err := b.SurfaceTokens().Set(context.Background(), keys[0], value); err != nil {
		t.Fatal(err)
	}
}

func preq(ref, purpose string, fresh bool) Request {
	return Request{Reference: ref, Purpose: purpose, Accept: acceptPassword, Fresh: fresh}
}

func resolved(t *testing.T, b *Backend, r Request) string {
	t.Helper()
	c, err := b.Resolve(context.Background(), r)
	if err != nil {
		t.Fatalf("resolve %s for %s: %v", r.Reference, r.Purpose, err)
	}
	return password(t, c)
}

func TestKeysPerPurpose(t *testing.T) {
	b := keyringBackend(t)
	for _, tc := range []struct {
		ref, purpose string
		want         []string
	}{
		{"support", "imap", []string{"uma/default/support/imap", "uma/default/support"}},
		{"support", "smtp.submission", []string{"uma/default/support/smtp.submission", "uma/default/support"}},
		{"wa/main", "meta.webhook.verify_token", []string{"uma/default/wa%2Fmain/meta.webhook.verify_token", "uma/default/wa%2Fmain"}},
		{"support", "", []string{"uma/default/support"}},
	} {
		got, err := b.Keys(tc.ref, tc.purpose)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("Keys(%q, %q) = %v, %v; want %v", tc.ref, tc.purpose, got, err, tc.want)
		}
	}
	if k, err := b.Key("support"); err != nil || k != "uma/default/support" {
		t.Errorf("Key = %q, %v", k, err)
	}
}

// TestPurposeEntryThenSharedEntry: a purpose with its own entry gets
// it; a purpose without one gets the reference's shared entry.
func TestPurposeEntryThenSharedEntry(t *testing.T) {
	b := keyringBackend(t)
	store(t, b, "support", envelope(t, "shared"))
	storePurpose(t, b, "support", "imap", envelope(t, "imap-only"))

	if got := resolved(t, b, preq("support", "imap", false)); got != "imap-only" {
		t.Errorf("imap resolved %q, want its own entry", got)
	}
	if got := resolved(t, b, preq("support", "smtp.submission", false)); got != "shared" {
		t.Errorf("smtp.submission resolved %q, want the shared entry", got)
	}
	d := b.Check(context.Background(), preq("support", "imap", false))
	if d.Status != StatusOK || d.Key != "uma/default/support/imap" {
		t.Errorf("Check imap = %+v", d)
	}
	d = b.Check(context.Background(), preq("support", "smtp.submission", false))
	if d.Status != StatusOK || d.Key != "uma/default/support" {
		t.Errorf("Check smtp.submission = %+v", d)
	}

	// Removing the purpose entry falls back on the next attempt.
	keys, _ := b.Keys("support", "imap")
	if err := b.SurfaceTokens().Delete(context.Background(), keys[0]); err != nil {
		t.Fatal(err)
	}
	if got := resolved(t, b, preq("support", "imap", false)); got != "shared" {
		t.Errorf("imap after its entry was deleted resolved %q, want the shared entry", got)
	}
}

// TestPurposeMissingNamesBothKeys: missing only when neither entry is
// stored, and the diagnosis names both keys, in the order tried.
func TestPurposeMissingNamesBothKeys(t *testing.T) {
	ctx := context.Background()
	b := keyringBackend(t)
	storePurpose(t, b, "support", "imap", envelope(t, seed))

	_, err := b.Resolve(ctx, preq("support", "graph", false))
	if model.CodeOf(err) != model.NotFound {
		t.Fatalf("resolve with neither entry: %v, want not_found", err)
	}
	noSeed(t, "missing error", err)
	d := b.Check(ctx, preq("support", "graph", false))
	want := []string{"uma/default/support/graph", "uma/default/support"}
	if d.Status != StatusMissing || !slices.Equal(d.Keys, want) || d.Key != "" {
		t.Fatalf("Check with neither entry = %+v", d)
	}
	for _, k := range want {
		if !strings.Contains(d.SuggestedFix, k) {
			t.Errorf("fix %q does not name %s", d.SuggestedFix, k)
		}
	}
	noSeed(t, "missing diagnosis", d)
}

// TestPurposesRotateIndependently: rotating or rejecting one purpose's
// entry leaves another's alone.
func TestPurposesRotateIndependently(t *testing.T) {
	ctx := context.Background()
	b := keyringBackend(t)
	storePurpose(t, b, "support", "imap", envelope(t, "imap-1"))
	storePurpose(t, b, "support", "smtp.submission", envelope(t, "smtp-1"))
	resolved(t, b, preq("support", "imap", false))
	resolved(t, b, preq("support", "smtp.submission", false))

	// The provider rejects imap; smtp.submission is rotated.
	if _, err := b.Resolve(ctx, preq("support", "imap", true)); model.CodeOf(err) != model.Unauthorized {
		t.Fatalf("fresh imap without rotation: %v, want unauthorized", err)
	}
	storePurpose(t, b, "support", "smtp.submission", envelope(t, "smtp-2"))
	if got := resolved(t, b, preq("support", "smtp.submission", true)); got != "smtp-2" {
		t.Errorf("fresh smtp.submission after rotation = %q", got)
	}
	if d := b.Check(ctx, preq("support", "imap", false)); d.Status != StatusRejected {
		t.Errorf("imap after smtp.submission rotated = %+v, want rejected", d)
	}
	if d := b.Check(ctx, preq("support", "smtp.submission", false)); d.Status != StatusOK {
		t.Errorf("smtp.submission = %+v, want ok", d)
	}

	storePurpose(t, b, "support", "imap", envelope(t, "imap-2"))
	if got := resolved(t, b, preq("support", "imap", true)); got != "imap-2" {
		t.Errorf("fresh imap after rotation = %q", got)
	}
	if got := resolved(t, b, preq("support", "smtp.submission", false)); got != "smtp-2" {
		t.Errorf("smtp.submission after imap rotated = %q", got)
	}
}

// TestRejectionIsPerPurpose: two purposes served by the shared entry
// are tracked apart: the provider rejecting it for one purpose says
// nothing about the other.
func TestRejectionIsPerPurpose(t *testing.T) {
	ctx := context.Background()
	b := keyringBackend(t)
	store(t, b, "support", envelope(t, "shared"))
	resolved(t, b, preq("support", "imap", false))

	if _, err := b.Resolve(ctx, preq("support", "imap", true)); model.CodeOf(err) != model.Unauthorized {
		t.Fatalf("fresh imap without rotation: %v, want unauthorized", err)
	}
	if d := b.Check(ctx, preq("support", "imap", false)); d.Status != StatusRejected {
		t.Errorf("imap = %+v, want rejected", d)
	}
	if d := b.Check(ctx, preq("support", "smtp.submission", false)); d.Status != StatusOK {
		t.Errorf("smtp.submission = %+v, want ok: only imap was rejected", d)
	}
	// Never served for smtp.submission before: a fresh request there is
	// not a rejection of it.
	if got := resolved(t, b, preq("support", "smtp.submission", true)); got != "shared" {
		t.Errorf("fresh smtp.submission = %q", got)
	}

	// imap gets its own entry: rotated for imap only.
	storePurpose(t, b, "support", "imap", envelope(t, "imap-own"))
	if got := resolved(t, b, preq("support", "imap", true)); got != "imap-own" {
		t.Errorf("fresh imap after its own entry was stored = %q", got)
	}
	if d := b.Check(ctx, preq("support", "imap", false)); d.Status != StatusOK {
		t.Errorf("imap after rotation = %+v", d)
	}
}

func TestPurposeCannotEscapeItsSegment(t *testing.T) {
	ctx := context.Background()
	b := keyringBackend(t)
	store(t, b, "support", envelope(t, "shared"))
	for _, p := range []string{".", "..", "a/b", "../support", "/imap", "imap/", ".hidden", "a b", "a%2Fb", "imap\x00", strings.Repeat("p", maxPurpose+1)} {
		if _, err := b.Keys("support", p); model.CodeOf(err) != model.Validation {
			t.Errorf("Keys(support, %q): %v, want validation", p, err)
		}
		if _, err := b.Resolve(ctx, preq("support", p, false)); model.CodeOf(err) != model.Validation {
			t.Errorf("resolve purpose %q: %v, want validation", p, err)
		}
		if d := b.Check(ctx, preq("support", p, false)); d.Status != StatusInvalid {
			t.Errorf("check purpose %q: %+v, want invalid", p, d)
		}
	}
}

func TestApsPurposeNames(t *testing.T) {
	ctx := context.Background()
	ref := profile.Ref{Scheme: profile.ApsScheme, ID: "noor"}
	path := apsProfile(t, "noor", "WA='"+string(envelope(t, "shared"))+"'\n"+
		"WA__META_APP_SECRET='"+string(envelope(t, "app-secret"))+"'\n"+
		"WA__META_WEBHOOK_VERIFY_TOKEN='"+string(envelope(t, "verify"))+"'\n")
	b := open(t, ref, fileConfig(t))

	for p, want := range map[string][]string{
		"meta.app_secret":           {"WA__META_APP_SECRET", "WA"},
		"meta.webhook.verify_token": {"WA__META_WEBHOOK_VERIFY_TOKEN", "WA"},
		"smtp.submission":           {"WA__SMTP_SUBMISSION", "WA"},
		"a-b@c+d":                   {"WA__A_B_C_D", "WA"},
		"":                          {"WA"},
	} {
		if got, err := b.Keys("WA", p); err != nil || !slices.Equal(got, want) {
			t.Errorf("Keys(WA, %q) = %v, %v; want %v", p, got, err, want)
		}
	}
	for p, want := range map[string]string{
		"meta.app_secret":           "app-secret",
		"meta.webhook.verify_token": "verify",
		"meta.graph":                "shared",
	} {
		if got := resolved(t, b, preq("WA", p, false)); got != want {
			t.Errorf("aps %s resolved %q, want %q", p, got, want)
		}
	}

	d := b.Check(ctx, preq("NONE", "meta.graph", false))
	if d.Status != StatusMissing || !slices.Equal(d.Keys, []string{"NONE__META_GRAPH", "NONE"}) {
		t.Fatalf("aps missing = %+v", d)
	}
	for _, s := range []string{"NONE__META_GRAPH", "NONE", path} {
		if !strings.Contains(d.SuggestedFix, s) {
			t.Errorf("fix %q does not name %s", d.SuggestedFix, s)
		}
	}
	if d := b.Check(ctx, preq("WA", "meta/graph", false)); d.Status != StatusInvalid {
		t.Errorf("aps purpose with '/': %+v", d)
	}
}

// TestChannelsResolvePerPurpose: uma's ports.Credentials view resolves
// the same way, one secret per purpose.
func TestChannelsResolvePerPurpose(t *testing.T) {
	b := keyringBackend(t)
	store(t, b, "wa", envelope(t, "shared"))
	storePurpose(t, b, "wa", "meta.app_secret", envelope(t, "app-secret"))
	creds := b.Channels()
	for p, want := range map[string]string{"meta.app_secret": "app-secret", "meta.graph": "shared"} {
		c, err := creds.Resolve(context.Background(), ports.CredentialRequest{
			Channel: model.ChannelRef{Tenant: "default", ID: "c1"}, Reference: "wa", Purpose: p, Accept: acceptPassword,
		})
		if err != nil || password(t, c) != want {
			t.Errorf("channel %s: %v, %v; want %q", p, c, err, want)
		}
	}
}
