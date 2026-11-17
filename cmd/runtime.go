package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/assist"
	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/blobs"
	"hop.top/heo/internal/composition"
	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/hooks"
	"hop.top/heo/internal/plugins"
	"hop.top/heo/internal/settings"
	kitlog "hop.top/kit/go/console/log"
	"hop.top/kit/go/console/ps"
	"hop.top/kit/go/console/serve"
	"hop.top/kit/go/runtime/bus"
	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// adapterRegistry registers every uma adapter this binary runs,
// explicitly, over the data store the daemon opened: the direct-message
// adapters (internal/composition), whose host settings read the loaded
// configuration, whose host secrets (the smtp.mail listener key)
// resolve in the profile's credential backend and whose own state (the
// widget's visitor logs) lives in st, plus a fake one in an e2e build
// (see extraAdapters).
func (a *app) adapterRegistry(st store.Store) (*adapters.Registry, error) {
	r := adapters.NewRegistry()
	o := composition.Options{Viper: a.viper, Credentials: a.openCredentials, Store: st}
	if v := a.viper(); v != nil {
		l := kitlog.New(v)
		l.SetOutput(os.Stderr)
		o.Logger = l
	}
	if err := composition.Register(r, o); err != nil {
		return nil, err
	}
	for _, ad := range extraAdapters() {
		if err := r.Register(ad); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// viper is the loaded configuration, nil before the root exists.
func (a *app) viper() *viper.Viper {
	if a.r == nil {
		return nil
	}
	return a.r.Viper
}

// openCredentials opens the loaded profile's credential backend.
func (a *app) openCredentials(ctx context.Context) (credentials.Resolver, error) {
	b, err := a.openBackend(ctx)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// openBackend opens the loaded profile's credential backend, as the
// configuration names it now: a restart in place re-reads it.
func (a *app) openBackend(ctx context.Context) (*credentials.Backend, error) {
	if a.state == nil {
		return nil, errors.New("configuration not loaded")
	}
	v := a.r.Viper
	return credentials.Open(ctx, credentials.Options{
		Profile: a.state.Profile(),
		Tenant:  model.TenantID(v.GetString(settings.KeyTenant)),
		Config:  credentials.ConfigFrom(v.GetString),
	})
}

// newRuntimeHost builds the runtime the workers, receivers and webhook
// services share. Its dependencies resolve from the loaded profile
// when the first of them starts.
func (a *app) newRuntimeHost() *daemon.Host {
	return daemon.New(daemon.Options{
		Viper:   func() *viper.Viper { return a.r.Viper },
		Resolve: a.runtimeDeps,
		Hub:     a.hub,
		// A plugin's operations run through the command tree as its own
		// actor (hookDispatcher).
		Hooks: daemon.HookOptions{Apply: &hooks.OpApplier{Dispatch: a.hookDispatcher(), Logger: hookLogger{a}}},
	})
}

// runtimeDeps resolves the runtime's dependencies from the profile: its
// tenant, its data store through the storage driver, its credential
// backend for channels, the adapters, its authorization policy and the
// host-wide provider account claims.
func (a *app) runtimeDeps(ctx context.Context) (daemon.Deps, error) {
	if a.state == nil {
		return daemon.Deps{}, errors.New("runtime: configuration not loaded")
	}
	v := a.r.Viper
	ref := a.state.Profile()
	tenant := model.TenantID(v.GetString(settings.KeyTenant))
	drv, scfg, err := storageConfig(ctx, a.storage, v, ref)
	if err != nil {
		return daemon.Deps{}, err
	}
	creds, err := credentials.Open(ctx, credentials.Options{
		Profile: ref,
		Tenant:  tenant,
		Config:  credentials.ConfigFrom(v.GetString),
	})
	if err != nil {
		return daemon.Deps{}, fmt.Errorf("credential backend: %w", err)
	}
	a.creds.Store(creds)
	claims, err := settings.AccountClaimsDir()
	if err != nil {
		return daemon.Deps{}, err
	}
	pol, err := a.authorization()
	if err != nil {
		return daemon.Deps{}, err
	}
	blobStore, err := blobs.Open(v.GetString(settings.KeyStorageBlobsDir))
	if err != nil {
		return daemon.Deps{}, err
	}
	logger := kitlog.New(v)
	logger.SetOutput(os.Stderr)
	opsRetention, err := time.ParseDuration(v.GetString(settings.KeyRetentionOperations))
	if err != nil {
		return daemon.Deps{}, fmt.Errorf("%s: %w", settings.KeyRetentionOperations, err)
	}
	pluginsDir, err := settings.PluginsDir(ref)
	if err != nil {
		return daemon.Deps{}, err
	}
	llmSeam()
	return daemon.Deps{
		PluginsDir: pluginsDir,
		PluginURL:  pluginURL(v),
		PluginToken: func(ctx context.Context, name string) (string, bool, error) {
			return credentials.LookupPluginToken(ctx, creds.SurfaceTokens(), ref, name)
		},
		Tenant:      tenant,
		Blobs:       blobStore,
		Owner:       settings.Tool + "/" + ref.Dir(),
		Storage:     daemon.DriverStorage(drv, scfg),
		Credentials: creds.Channels(),
		HTTP:        composition.NewHTTPClient(httpBase()),
		Adapters:    a.adapterRegistry,
		Authorizer:  pol,
		// Identities link on the desk actor's attestation alone: heo
		// verifies no other evidence kind.
		IdentityVerifiers: desk.IdentityVerifiers(),
		Accounts:          daemon.NewAccounts(claims),
		RequireDurable:    true,
		Retention:         uma.RetentionPolicy{Operations: opsRetention},
		Held:              heldPolicy(v),
		Logger:            logger,
	}, nil
}

// runtimeCheck feeds the runtime services', the authorization
// policy's and the adapters' validation to `config validate`. Only a configured smtp.mail listener
// certificate reads anything beyond the configuration: its chain file,
// and its key from the credential backend, within checkTimeout.
const checkTimeout = 15 * time.Second

func (a *app) runtimeCheck(v *viper.Viper, report func(settings.Severity, string, string)) {
	sev := func(warning bool) settings.Severity {
		if warning {
			return settings.SeverityWarning
		}
		return settings.SeverityError
	}
	for _, p := range daemon.Check(v) {
		report(sev(p.Warning), p.Key, p.Message)
	}
	for _, p := range authz.Check(v, authz.OwnerActor(), daemon.ServiceActor) {
		report(sev(p.Warning), p.Key, p.Message)
	}
	for _, p := range plugins.Check(v) {
		report(sev(p.Warning), p.Key, p.Message)
	}
	for _, p := range assist.Check(v) {
		report(sev(p.Warning), p.Key, p.Message)
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	for _, p := range composition.Check(ctx, v, a.openCredentials) {
		report(sev(p.Warning), p.Key, p.Message)
	}
}

// pidfile writes the profile's daemon pidfile once every service of a
// `service start` run has reported ready, and removes it when that run
// ends. A daemon refused at start never writes it, so it never
// clobbers the pidfile of the daemon that refused it.
type pidfile struct {
	a     *app
	ready bus.Topic
	path  string
}

func newPIDFile(a *app) *pidfile {
	return &pidfile{a: a, ready: serve.DefaultTopics(serve.DefaultTopicPrefix)[serve.ObjectSupervisor+"."+serve.ActionReadyReported]}
}

// Publish is the supervisor's lifecycle publisher.
func (p *pidfile) Publish(_ context.Context, e bus.Event) error {
	if e.Topic != p.ready || p.a.state == nil {
		return nil
	}
	// Only the daemon names itself there: the process that serves the
	// control socket or runs the runtime. An MCP stdio relay, or a
	// lone REST listener, for the same profile is not it.
	if !p.a.control.servesControl() && !p.a.runtime.StorageReady() {
		return nil
	}
	path, err := settings.PIDPath(p.a.state.Profile())
	if err != nil {
		return err
	}
	if err := ps.WritePIDFile(path, ps.Entry{ID: strconv.Itoa(os.Getpid())}); err != nil {
		return err
	}
	p.path = path
	return nil
}

// remove deletes the pidfile when it still names this process.
func (p *pidfile) remove() {
	if p.path == "" {
		return
	}
	b, err := os.ReadFile(p.path)
	if err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(os.Getpid()) {
		_ = os.Remove(p.path)
	}
	p.path = ""
}

// heldPolicy opts the local writes heo holds into uma's holding when
// held_operations.enabled is on: a label create and a conversation
// transition. A start creates its conversation when approved, so a
// rejected one leaves nothing behind.
func heldPolicy(v *viper.Viper) uma.HeldPolicy {
	if !v.GetBool(settings.KeyHeldEnabled) {
		return uma.HeldPolicy{}
	}
	return uma.HeldPolicy{LabelConfigure: true, ConversationTransition: true}
}

// pluginURL is where a hook plugin reaches heo's HTTP surface: the REST
// surface when it is enabled, empty otherwise.
func pluginURL(v *viper.Viper) string {
	if !v.GetBool("services.api.enabled") {
		return ""
	}
	if addr := v.GetString("services.api.addr"); addr != "" {
		return surfaceURL("api", addr)
	}
	return ""
}
