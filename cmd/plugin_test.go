package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/profile"
	"hop.top/kit/go/storage/secret/memory"
	"hop.top/uma/model"
)

// TestBearerSubject: a surface token acts as its minted uid, a plugin
// token as its plugin actor only, a revoked plugin token is refused,
// and a plugin token needs no surface token to be configured.
func TestBearerSubject(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	ref := profile.Default()
	now := time.Now()
	surfaceKey := credentials.SurfaceTokenKey(ref)
	surface, err := credentials.MintSurfaceToken(ctx, st, surfaceKey, "uid:501", now)
	if err != nil {
		t.Fatal(err)
	}
	plug, err := credentials.MintPluginToken(ctx, st, ref, "triage", now)
	if err != nil {
		t.Fatal(err)
	}

	if got, err := bearerSubject(ctx, st, ref, surfaceKey, surface); err != nil || got != "uid:501" {
		t.Errorf("surface token = %q, %v", got, err)
	}
	if got, err := bearerSubject(ctx, st, ref, surfaceKey, plug); err != nil || got != "plugin:triage" {
		t.Errorf("plugin token = %q, %v", got, err)
	}
	if got, err := bearerSubject(ctx, st, ref, "", plug); err != nil || got != "plugin:triage" {
		t.Errorf("plugin token without a surface token ref = %q, %v", got, err)
	}
	if _, err := bearerSubject(ctx, st, ref, "", surface); err == nil {
		t.Error("surface token accepted with no ref configured")
	}
	if err := credentials.RevokePluginToken(ctx, st, ref, "triage"); err != nil {
		t.Fatal(err)
	}
	if _, err := bearerSubject(ctx, st, ref, surfaceKey, plug); !errors.Is(err, credentials.ErrSurfaceTokenRefused) {
		t.Errorf("revoked plugin token = %v, want refused", err)
	}
	if got, err := bearerSubject(ctx, st, ref, surfaceKey, surface); err != nil || got != "uid:501" {
		t.Errorf("surface token after a plugin revoke = %q, %v", got, err)
	}
}

// TestPluginTokenIsBoundedByItsGrants: the token a plugin process holds
// acts as plugin:<name> on every HTTP surface (surfaceAuth reads the
// subject with bearerSubject), and what that actor may do is what
// authorization.grants gives it: an observer reads and cannot write, a
// plugin with no grant can do nothing, and a plugin's token never
// acts as another plugin or as the owner.
func TestPluginTokenIsBoundedByItsGrants(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	ref := profile.Default()
	now := time.Now()
	surfaceKey := credentials.SurfaceTokenKey(ref)
	observer, _ := credentials.MintPluginToken(ctx, st, ref, "watcher", now)
	nogrant, _ := credentials.MintPluginToken(ctx, st, ref, "bare", now)
	pol := authz.New(authz.Config{Owner: []authz.Role{authz.RoleOperator, authz.RoleDesk}, Grants: []authz.Grant{
		{Actor: "plugin:watcher", Roles: []authz.Role{authz.RoleObserver}},
	}}, "acme", "uid:501", "svc:heo-runtime")

	actAs := func(tok string) model.Principal {
		t.Helper()
		subj, err := bearerSubject(ctx, st, ref, surfaceKey, tok)
		if err != nil {
			t.Fatal(err)
		}
		return model.Principal{Tenant: "acme", Actor: subj}
	}
	w := actAs(observer)
	if w.Actor != "plugin:watcher" {
		t.Fatalf("acts as %s", w.Actor)
	}
	if ok, why := pol.Permits(w, model.ActionMessageRead); !ok {
		t.Errorf("observer cannot read: %s", why)
	}
	for _, verb := range []string{model.ActionMessageNote, model.ActionMessageReply, model.ActionConversationAssign,
		model.ActionConversationTransition, model.ActionChannelConfigure, model.ActionPrivacyRedact} {
		if ok, _ := pol.Permits(w, verb); ok {
			t.Errorf("observer plugin may %s", verb)
		}
	}
	b := actAs(nogrant)
	for _, verb := range []string{model.ActionMessageRead, model.ActionMessageNote} {
		if ok, _ := pol.Permits(b, verb); ok {
			t.Errorf("plugin with no grant may %s", verb)
		}
	}
	// Presenting another plugin's name with this one's secret is refused.
	_, secret, _ := strings.Cut(observer, ".")
	if _, err := bearerSubject(ctx, st, ref, surfaceKey, "bare."+secret); err == nil {
		t.Error("a plugin's secret under another name accepted")
	}
}

func TestPluginURL(t *testing.T) {
	v := viper.New()
	if got := pluginURL(v); got != "" {
		t.Errorf("api off: %q", got)
	}
	v.Set("services.api.enabled", true)
	v.Set("services.api.addr", "127.0.0.1:8421")
	if got := pluginURL(v); got != "http://127.0.0.1:8421" {
		t.Errorf("api on: %q", got)
	}
}
