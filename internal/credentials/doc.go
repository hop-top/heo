// Package credentials resolves credential references for one heo
// profile.
//
// Configuration holds references only, never credential material. A
// profile has exactly one credential backend:
//
//   - a native profile (heo://profile/<name>) resolves references in a
//     kit secret store chosen by credentials.backend: keyring (the
//     default), file, agefile or openbao. Each backend is constructed
//     directly; kit's env backend is never linked. A reference r is
//     stored under uma/<tenant>/<r> as a uma credential envelope (see
//     hop.top/uma/contrib/kit/secret).
//   - an aps-bound profile (aps://profile/<id>) resolves references in
//     the aps profile's secrets file, <aps data>/profiles/<id>/
//     secrets.env, where a reference is a secret name and its value
//     the same envelope. heo only reads it.
//
// A reference resolves per purpose: the purpose's own entry
// (uma/<tenant>/<r>/<purpose>, or <r>__<PURPOSE> in an aps secrets
// file) when one is stored, else the reference's shared entry, so the
// secrets of one channel rotate independently (see Backend.Keys).
//
// Every resolution reads the store: a credential rotated there is used
// on the next attempt. A Fresh request — the provider rejected the
// last credential for that reference and purpose — gets
// model.Unauthorized while the stored credential is still the rejected
// one, and Check reports it as rejected for that purpose until it is
// rotated.
//
// An entry may hold an OAuth grant instead of a credential: kit's grant
// envelope (client_credentials, client_assertion, jwt_bearer or
// refresh_token; see hop.top/kit/go/storage/secret/oauth). The store
// keeps the grant; the backend resolves it to an oauth2 credential, a
// fresh access token minted through kit's token source and refreshed
// shortly before it expires. On a writable backend (keyring, file,
// openbao) the access token is cached in the store as a uma oauth2
// envelope and a rotated refresh token is written back. An aps
// profile's secrets and the agefile backend cannot be written: the
// token is cached in memory only and a refresh_token grant is refused,
// by RefuseGrant (shown by `config validate`) and when resolved. A
// Fresh request after the provider rejected the token drops it once
// and serves a new one; a token the endpoint issues again unchanged is
// model.Unauthorized. Check reports the grant kind and the access
// token's expiry.
//
// Errors and diagnoses carry fixed text, references and store keys,
// never material: store errors are dropped, not wrapped.
//
// The profile's HTTP-surface token lives in heo's kit store under
// SurfaceTokenKey, for aps-bound profiles too, since their secrets are
// read-only to heo.
package credentials
