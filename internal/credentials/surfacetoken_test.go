package credentials

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"hop.top/kit/go/storage/secret/memory"
	"hop.top/uma/model"
)

var mintedAt = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// TestSurfaceTokenMintVerifyRotate: a minted token is 32 random bytes,
// verifies as its subject, and stops verifying once rotated; anything
// else presented is refused.
func TestSurfaceTokenMintVerifyRotate(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	key := SurfaceTokenKey(native)

	if _, ok, err := LookupSurfaceToken(ctx, st, key); err != nil || ok {
		t.Fatalf("lookup before mint = %v, %v", ok, err)
	}
	if _, err := VerifySurfaceToken(ctx, st, key, "anything"); !errors.Is(err, ErrSurfaceTokenRefused) {
		t.Fatalf("verify with no token stored = %v, want ErrSurfaceTokenRefused", err)
	}

	tok, err := MintSurfaceToken(ctx, st, key, "uid:501", mintedAt)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token %q decodes to %d bytes (%v), want 32", tok, len(raw), err)
	}
	meta, ok, err := LookupSurfaceToken(ctx, st, key)
	if err != nil || !ok || meta.Subject != "uid:501" || !meta.MintedAt.Equal(mintedAt) {
		t.Fatalf("lookup = %+v, %v, %v", meta, ok, err)
	}
	got, err := VerifySurfaceToken(ctx, st, key, tok)
	if err != nil || got.Subject != model.ActorID("uid:501") {
		t.Fatalf("verify minted = %+v, %v", got, err)
	}
	if v, err := RevealSurfaceToken(ctx, st, key); err != nil || v != tok {
		t.Fatalf("reveal = %q, %v", v, err)
	}
	for _, bad := range []string{"", tok + "x", tok[:len(tok)-1], strings.ToUpper(tok)} {
		if _, err := VerifySurfaceToken(ctx, st, key, bad); !errors.Is(err, ErrSurfaceTokenRefused) {
			t.Errorf("verify %q = %v, want ErrSurfaceTokenRefused", bad, err)
		}
	}

	next, err := MintSurfaceToken(ctx, st, key, "uid:502", mintedAt.Add(time.Hour))
	if err != nil || next == tok {
		t.Fatalf("rotate = %q, %v", next, err)
	}
	if _, err := VerifySurfaceToken(ctx, st, key, tok); !errors.Is(err, ErrSurfaceTokenRefused) {
		t.Errorf("old token after rotation = %v, want refused", err)
	}
	if got, err := VerifySurfaceToken(ctx, st, key, next); err != nil || got.Subject != "uid:502" {
		t.Errorf("new token = %+v, %v", got, err)
	}
}

// TestSurfaceTokenErrorsCarryNoMaterial: no error, refusal or record
// rendering carries the stored or the presented token.
func TestSurfaceTokenErrorsCarryNoMaterial(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	key := SurfaceTokenKey(native)
	tok, err := MintSurfaceToken(ctx, st, key, "uid:501", mintedAt)
	if err != nil {
		t.Fatal(err)
	}
	_, err = VerifySurfaceToken(ctx, st, key, seed)
	meta, _, _ := LookupSurfaceToken(ctx, st, key)
	for what, v := range map[string]any{"refusal": err, "metadata": meta} {
		for _, s := range []string{tok, seed} {
			if strings.Contains(strings.Join([]string{errString(v), sprint(v)}, " "), s) {
				t.Errorf("%s carries material: %v", what, v)
			}
		}
	}

	// A malformed record is reported as such, without its content.
	if err := st.Set(ctx, key, []byte(seed)); err != nil {
		t.Fatal(err)
	}
	_, _, err = LookupSurfaceToken(ctx, st, key)
	if err == nil || strings.Contains(err.Error(), seed) || !strings.Contains(err.Error(), "rotate") {
		t.Errorf("malformed record = %v", err)
	}
	if _, err := VerifySurfaceToken(ctx, st, key, seed); err == nil || strings.Contains(err.Error(), seed) {
		t.Errorf("verify against malformed record = %v", err)
	}
}

func TestSurfaceTokenRefShape(t *testing.T) {
	if err := ValidSurfaceTokenRef("heo/default/surface-token"); err != nil {
		t.Errorf("default ref refused: %v", err)
	}
	for _, bad := range []string{"", " ", "/heo/x", "heo//x", "heo/../x", "heo/./x", "heo/x/", "heo/a b", "heo/x\n"} {
		if err := ValidSurfaceTokenRef(bad); err == nil {
			t.Errorf("ref %q accepted", bad)
		}
	}
}

func TestConfigValidatesSurfaceTokenRef(t *testing.T) {
	c := fileConfig(t)
	c.SurfaceTokenRef = "heo/../x"
	var found bool
	for _, p := range c.Validate() {
		if p.Key == KeySurfaceTokenRef && !p.Warning {
			found = true
		}
	}
	if !found {
		t.Errorf("invalid %s not refused: %+v", KeySurfaceTokenRef, c.Validate())
	}
	c.SurfaceTokenRef = SurfaceTokenKey(native)
	for _, p := range c.Validate() {
		if p.Key == KeySurfaceTokenRef {
			t.Errorf("valid ref refused: %+v", p)
		}
	}
}

func errString(v any) string {
	if e, ok := v.(error); ok && e != nil {
		return e.Error()
	}
	return ""
}

func sprint(v any) string { return fmt.Sprintf("%v %+v %#v", v, v, v) }
