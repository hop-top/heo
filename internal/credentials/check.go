package credentials

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"time"

	kitsecret "hop.top/kit/go/storage/secret"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Status is the verdict of Check.
type Status string

// Statuses.
const (
	// StatusOK: a usable credential is stored.
	StatusOK Status = "ok"
	// StatusMissing: nothing is stored under the reference — never
	// stored, deleted or revoked.
	StatusMissing Status = "missing"
	// StatusRejected: the provider rejected the stored credential and
	// it has not been rotated since.
	StatusRejected Status = "rejected"
	// StatusExpired: the stored credential is past its expiry.
	StatusExpired Status = "expired"
	// StatusMalformed: what is stored cannot be decoded.
	StatusMalformed Status = "malformed"
	// StatusUnsupported: the stored kind is not one the caller accepts,
	// or the store cannot read secrets.
	StatusUnsupported Status = "unsupported"
	// StatusInvalid: the reference itself is not valid.
	StatusInvalid Status = "invalid"
	// StatusUnavailable: the store could not be read.
	StatusUnavailable Status = "unavailable"
)

// Diagnosis describes the credential behind one reference without any
// of its material: suitable for output, logs and events. Keys are the
// store keys looked under, in order (the purpose's own entry, then the
// entry every purpose shares); Key is the one holding the credential,
// when one does.
type Diagnosis struct {
	Reference string               `json:"reference"               yaml:"reference"`
	Purpose   string               `json:"purpose,omitempty"       yaml:"purpose,omitempty"`
	Backend   string               `json:"backend"                 yaml:"backend"`
	Keys      []string             `json:"keys,omitempty"          yaml:"keys,omitempty"`
	Key       string               `json:"key,omitempty"           yaml:"key,omitempty"`
	Status    Status               `json:"status"                  yaml:"status"`
	Kind      ports.CredentialKind `json:"kind,omitempty"          yaml:"kind,omitempty"`
	// GrantKind is set when the entry holds an OAuth grant: how the
	// backend mints the access token (client_credentials,
	// client_assertion, jwt_bearer or refresh_token). Kind is then
	// oauth2, the credential the grant yields.
	GrantKind string `json:"grant_kind,omitempty" yaml:"grant_kind,omitempty"`
	// ExpiresAt is the credential's expiry; for a grant, that of the
	// access token minted (or cached) for this check.
	ExpiresAt    *time.Time `json:"expires_at,omitempty"    yaml:"expires_at,omitempty"`
	RejectedAt   *time.Time `json:"rejected_at,omitempty"   yaml:"rejected_at,omitempty"`
	Message      string     `json:"message"                 yaml:"message"`
	SuggestedFix string     `json:"suggested_fix,omitempty" yaml:"suggested_fix,omitempty"`
}

// allKinds is what Check accepts when the request names no kind.
var allKinds = []ports.CredentialKind{
	ports.KindSecret, ports.KindPassword, ports.KindOAuth2, ports.KindClientCert, ports.KindKerberos,
}

// Check diagnoses the credential behind req.Reference for req.Purpose:
// stored under the purpose's own key or the shared one (see Keys),
// decodable, of an accepted kind (any kind when req.Accept is empty),
// unexpired, and not rejected by the provider since it was last
// stored. It reads the store like Resolve, keeps nothing and never
// reports material; req.Fresh is ignored. This is what a channel check
// reports per credential reference and purpose.
func (b *Backend) Check(ctx context.Context, req Request) Diagnosis {
	d := Diagnosis{Reference: req.Reference, Purpose: req.Purpose, Backend: b.name}
	pr := b.portsRequest(req)
	pr.Fresh = false
	if len(pr.Accept) == 0 {
		pr.Accept = allKinds
	}
	keys, err := b.keysFor(pr)
	if err != nil {
		d.Status, d.Message = StatusInvalid, safeMessage(err)
		return d
	}
	d.Keys = keys
	key, err := locate(ctx, b.store, keys)
	switch {
	case errors.Is(err, kitsecret.ErrNotFound):
		d.Status, d.Message = StatusMissing, "no credential stored (never stored, deleted or revoked)"
	case err != nil:
		d.Status, d.Message = storeProblem(err)
	}
	if d.Status != "" {
		d.SuggestedFix = b.fix(d.Status, keys)
		if el := (*earlierLayout)(nil); errors.As(err, &el) {
			d.SuggestedFix = el.fix()
		}
		return d
	}
	d.Key = key
	// The rest diagnoses the entry located, the one Resolve reads.
	keys = []string{key}

	cred, gs, err := b.readEntry(ctx, pr, keys)
	var ge *grantError
	switch {
	case errors.As(err, &ge):
		d.GrantKind = string(ge.kind)
		d.Status, d.Message, d.SuggestedFix = ge.status, ge.me.SafeMessage, ge.fix
		return d
	case err != nil:
		d.Status, d.Message = statusOf(err), safeMessage(err)
		d.SuggestedFix = b.fix(d.Status, keys)
		return d
	}
	defer zero(cred)
	d.Kind = cred.Kind()
	if gs != nil {
		d.GrantKind = string(gs.grant.Kind)
	}
	if exp := cred.Expiry(); !exp.IsZero() {
		d.ExpiresAt = &exp
		if !b.now().Before(exp) {
			d.Status, d.Message = StatusExpired, "credential expired"
			d.SuggestedFix = b.fix(d.Status, keys)
			return d
		}
	}
	fp := b.fingerprint(cred)
	b.mu.Lock()
	r, rejected := b.rejected[slot{req.Reference, req.Purpose}]
	b.mu.Unlock()
	if rejected && hmac.Equal(r.fp[:], fp[:]) {
		at := r.at
		d.Status, d.RejectedAt = StatusRejected, &at
		d.Message = "the provider rejected this credential and it has not been rotated since"
		d.SuggestedFix = b.fix(d.Status, keys)
		return d
	}
	d.Status, d.Message = StatusOK, "credential present"
	return d
}

// statusOf maps a resolver error onto a Status.
func statusOf(err error) Status {
	switch model.CodeOf(err) {
	case model.NotFound:
		return StatusMissing
	case model.Permanent:
		return StatusMalformed
	case model.Unsupported:
		return StatusUnsupported
	case model.Validation, model.Unauthorized:
		return StatusInvalid
	}
	return StatusUnavailable
}

// safeMessage returns the fixed text of a model error; anything else
// is reported generically.
func safeMessage(err error) string {
	var me *model.Error
	if errors.As(err, &me) && me.SafeMessage != "" {
		return me.SafeMessage
	}
	return "credential store unavailable"
}

func (b *Backend) fix(s Status, keys []string) string {
	where := b.where(keys)
	switch s {
	case StatusMissing, StatusExpired, StatusRejected:
		return "store a new credential in " + where
	case StatusMalformed:
		if b.name == BackendAps {
			return fmt.Sprintf("fix %s; quote JSON credential envelopes in single quotes", where)
		}
		return "replace the value in " + where + " with a valid credential envelope"
	case StatusUnsupported:
		return "store a credential of a kind the channel accepts in " + where
	case StatusUnavailable:
		return fmt.Sprintf("check that the %s backend is reachable and readable", b.name)
	}
	return ""
}
