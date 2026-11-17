package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/viper"

	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/profile"
	"hop.top/heo/internal/settings"
	"hop.top/heo/internal/storage"
	"hop.top/heo/internal/storage/postgres"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// newStorageRegistry registers every storage driver this binary can run
// on, explicitly: adding a driver is one line here plus passing the
// shared conformance suite.
func newStorageRegistry() *storage.Registry {
	r := storage.NewRegistry()
	for _, d := range []storage.Driver{
		postgres.New(postgres.Options{Runner: storageRunner()}),
	} {
		if err := r.Register(d); err != nil {
			panic(err)
		}
	}
	return r
}

// storageCheck feeds the registry's offline storage validation to
// `config validate`.
func (a *app) storageCheck(v *viper.Viper, report func(settings.Severity, string, string)) {
	for _, i := range a.storage.Check(v) {
		sev := settings.SeverityError
		if i.Warning {
			sev = settings.SeverityWarning
		}
		report(sev, i.Key, i.Message)
	}
}

// storageConfig selects the profile's storage driver and builds what it
// connects with: its settings, the backup directory, an owner name for
// ownership diagnostics, and the profile's credential backend, the same
// one channels resolve through, for the driver's credential references.
func storageConfig(ctx context.Context, reg *storage.Registry, v *viper.Viper, ref profile.Ref) (storage.Driver, storage.Config, error) {
	d, s, err := reg.Select(v)
	if err != nil {
		return nil, storage.Config{}, err
	}
	b, err := credentials.Open(ctx, credentials.Options{
		Profile: ref,
		Tenant:  model.TenantID(v.GetString(settings.KeyTenant)),
		Config:  credentials.ConfigFrom(v.GetString),
	})
	if err != nil {
		return nil, storage.Config{}, fmt.Errorf("credential backend: %w", err)
	}
	return d, storage.Config{
		Settings:    s,
		Credentials: diagnosedCredentials{b},
		BackupDir:   v.GetString(settings.KeyStorageBackupDir),
		Owner:       settings.Tool + "/" + ref.Dir(),
	}, nil
}

// diagnosedCredentials resolves through the profile's credential
// backend; a failed resolution carries the backend's diagnosis of the
// reference (status, store key, fix) instead of the bare store error.
// Neither carries material.
type diagnosedCredentials struct{ b *credentials.Backend }

func (c diagnosedCredentials) Resolve(ctx context.Context, req storage.CredentialRequest) (ports.Credential, error) {
	cred, err := c.b.Resolve(ctx, req)
	if err == nil || ctx.Err() != nil {
		return cred, err
	}
	d := c.b.Check(ctx, req)
	msg := fmt.Sprintf("credential %q for %s: %s: %s", d.Reference, d.Purpose, d.Status, d.Message)
	if d.SuggestedFix != "" {
		msg += "; " + d.SuggestedFix
	}
	return nil, model.NewError(model.CodeOf(err), msg)
}
