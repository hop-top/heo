package cmd

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/profile"
	"hop.top/heo/internal/settings"
	"hop.top/kit/go/console/output"
	kitsecret "hop.top/kit/go/storage/secret"
	"hop.top/kit/go/transport/api"
	"hop.top/uma/model"
)

// errSurfaceAuth is every refusal surfaceAuth answers a caller with:
// it never says whether a token was missing, wrong or not configured.
var errSurfaceAuth = errors.New("authentication required: present the profile's surface token as a bearer token")

// surfaceAuth is the authenticator REST, gRPC and MCP over HTTP share
// (one credential, one verdict path): the profile's surface token
// (credentials.surface_token_ref), presented as
// `Authorization: Bearer <token>`. An accepted request acts as the
// token's subject, uid:<n> of the user who minted it, in the
// profile's tenant; a plugin's token (`heo plugin token mint`) acts as
// plugin:<name>. Kit uses it only where services.<svc>.auth.mode
// (or services.all.auth.mode) selects no kit authenticator.
//
// The store is read on every request, so a rotated token is refused
// from the next request on. A store that cannot be read refuses too:
// authentication fails closed.
func (a *app) surfaceAuth(r *http.Request) (any, error) {
	presented, ok := bearerToken(r)
	if !ok || a.state == nil {
		return nil, errSurfaceAuth
	}
	v := a.r.Viper
	b, err := a.openBackend(r.Context())
	if err != nil {
		return nil, errSurfaceAuth
	}
	subject, err := bearerSubject(r.Context(), b.SurfaceTokens(), a.state.Profile(), settings.SurfaceTokenRef(v), presented)
	if err != nil {
		return nil, errSurfaceAuth
	}
	return api.Claims{Subject: string(subject), Tenant: v.GetString(settings.KeyTenant)}, nil
}

// bearerSubject is the actor a presented bearer token acts as. A
// plugin's token ("<name>.<secret>") acts as plugin:<name> and as no
// other actor, and needs no surface token; anything else is the
// profile's surface token, which acts as the uid that minted it.
func bearerSubject(ctx context.Context, st kitsecret.Store, ref profile.Ref, surfaceRef, presented string) (model.ActorID, error) {
	if credentials.IsPluginToken(presented) {
		tok, err := credentials.VerifyPluginToken(ctx, st, ref, presented)
		return tok.Subject, err
	}
	if surfaceRef == "" {
		return "", errSurfaceAuth
	}
	tok, err := credentials.VerifySurfaceToken(ctx, st, surfaceRef, presented)
	return tok.Subject, err
}

// bearerToken returns the request's bearer token: the Authorization
// header's credentials under the Bearer scheme (case-insensitive, RFC
// 9110 §11.1).
func bearerToken(r *http.Request) (string, bool) {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return "", false
	}
	return tok, true
}

// checkSurfaceToken refuses a start that would serve svc on the
// surface token while none is stored: every request would be refused.
func (a *app) checkSurfaceToken(c *cobra.Command, svc string) error {
	ref := settings.SurfaceTokenRef(a.r.Viper)
	b, err := a.openBackend(c.Context())
	if err != nil {
		return err
	}
	if _, ok, err := credentials.LookupSurfaceToken(c.Context(), b.SurfaceTokens(), ref); err != nil {
		return err
	} else if !ok {
		e := output.UsageError("services." + svc + ": no surface token stored under " + ref)
		e.SuggestedFix = "run `heo service setup " + svc + "` to mint one"
		return e
	}
	return nil
}
