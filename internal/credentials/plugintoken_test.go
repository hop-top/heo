package credentials

import (
	"context"
	"errors"
	"strings"
	"testing"

	"hop.top/kit/go/storage/secret/memory"
	"hop.top/uma/model"
)

// TestPluginTokenActsOnlyAsItsActor: a minted token verifies as
// plugin:<name> and as no other actor; a token minted for one plugin
// presented under another's name, or with a forged name, is refused.
func TestPluginTokenActsOnlyAsItsActor(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	a, err := MintPluginToken(ctx, st, native, "alpha", mintedAt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := MintPluginToken(ctx, st, native, "beta", mintedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, "alpha.") || !strings.HasPrefix(b, "beta.") {
		t.Fatalf("tokens %q, %q do not carry the plugin name", a, b)
	}
	got, err := VerifyPluginToken(ctx, st, native, a)
	if err != nil || got.Subject != model.ActorID("plugin:alpha") {
		t.Fatalf("verify alpha = %+v, %v", got, err)
	}
	if got, err := VerifyPluginToken(ctx, st, native, b); err != nil || got.Subject != "plugin:beta" {
		t.Fatalf("verify beta = %+v, %v", got, err)
	}
	// alpha's secret under beta's name, and the reverse.
	secretA := strings.TrimPrefix(a, "alpha.")
	for _, forged := range []string{"beta." + secretA, "gamma." + secretA, "alpha.", ".x", "alpha", "", "../x." + secretA, "alpha." + secretA + "x"} {
		if _, err := VerifyPluginToken(ctx, st, native, forged); !errors.Is(err, ErrSurfaceTokenRefused) {
			t.Errorf("verify %q = %v, want refused", forged, err)
		}
	}
}

// TestPluginTokenRevoked: revoking stops the token verifying at once,
// leaves the other plugins' tokens alone, and a re-mint replaces it.
func TestPluginTokenRevoked(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	a, _ := MintPluginToken(ctx, st, native, "alpha", mintedAt)
	b, _ := MintPluginToken(ctx, st, native, "beta", mintedAt)
	if err := RevokePluginToken(ctx, st, native, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPluginToken(ctx, st, native, a); !errors.Is(err, ErrSurfaceTokenRefused) {
		t.Errorf("revoked token = %v, want refused", err)
	}
	if _, err := VerifyPluginToken(ctx, st, native, b); err != nil {
		t.Errorf("other plugin's token = %v", err)
	}
	if err := RevokePluginToken(ctx, st, native, "alpha"); err != nil {
		t.Errorf("revoking twice = %v, want nil", err)
	}
	a2, err := MintPluginToken(ctx, st, native, "alpha", mintedAt)
	if err != nil || a2 == a {
		t.Fatalf("re-mint = %q, %v", a2, err)
	}
	if _, err := VerifyPluginToken(ctx, st, native, a); !errors.Is(err, ErrSurfaceTokenRefused) {
		t.Errorf("old token after re-mint = %v, want refused", err)
	}
}

// TestPluginTokenNames: only a plain name is accepted, so a name never
// escapes its store key.
func TestPluginTokenNames(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	for _, bad := range []string{"", "Alpha", "a.b", "a/b", "..", "-a", "a b", "svc:x", strings.Repeat("a", 64)} {
		if _, err := MintPluginToken(ctx, st, native, bad, mintedAt); err == nil {
			t.Errorf("mint %q accepted", bad)
		}
		if err := RevokePluginToken(ctx, st, native, bad); err == nil {
			t.Errorf("revoke %q accepted", bad)
		}
	}
	// A profile surface token is not a plugin token, and the reverse.
	tok, _ := MintSurfaceToken(ctx, st, SurfaceTokenKey(native), "uid:501", mintedAt)
	if _, err := VerifyPluginToken(ctx, st, native, tok); !errors.Is(err, ErrSurfaceTokenRefused) {
		t.Errorf("surface token as plugin token = %v", err)
	}
}

// TestValidPluginName: a name is a lowercase letter, then lowercase
// letters, digits and '-', at most 63 in all (docs/manual/hooks.md).
func TestValidPluginName(t *testing.T) {
	for _, ok := range []string{"a", "triage", "a1", "a-b-9", "x" + strings.Repeat("y", 62)} {
		if err := ValidPluginName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1abc", "9", "-a", "A", "a_b", "a.b", "a b", "x" + strings.Repeat("y", 63)} {
		if err := ValidPluginName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestLookupPluginToken: the daemon reads back what the plugin presents,
// verifying; none minted is not an error; a revoked one is none again.
func TestLookupPluginToken(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	if tok, ok, err := LookupPluginToken(ctx, st, native, "triage"); tok != "" || ok || err != nil {
		t.Fatalf("none minted: %q %v %v", tok, ok, err)
	}
	minted, err := MintPluginToken(ctx, st, native, "triage", mintedAt)
	if err != nil {
		t.Fatal(err)
	}
	tok, ok, err := LookupPluginToken(ctx, st, native, "triage")
	if err != nil || !ok || tok != minted {
		t.Fatalf("lookup = %q %v %v, want %q", tok, ok, err, minted)
	}
	if _, err := VerifyPluginToken(ctx, st, native, tok); err != nil {
		t.Errorf("looked-up token refused: %v", err)
	}
	if other, ok, _ := LookupPluginToken(ctx, st, native, "other"); ok || other != "" {
		t.Error("another plugin's token returned")
	}
	_ = RevokePluginToken(ctx, st, native, "triage")
	if _, ok, _ := LookupPluginToken(ctx, st, native, "triage"); ok {
		t.Error("revoked token returned")
	}
}
