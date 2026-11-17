package settings

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/approvechat"
	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/trustcfg"
	"hop.top/uma/model"
)

// Severity of a validation issue.
type Severity string

const (
	// SeverityError makes the configuration unusable.
	SeverityError Severity = "error"
	// SeverityWarning flags a weaker-than-default setting.
	SeverityWarning Severity = "warning"
)

// Issue is one validation finding.
type Issue struct {
	Severity Severity `json:"severity" yaml:"severity" table:"SEVERITY"`
	Key      string   `json:"key"      yaml:"key"      table:"KEY"`
	Message  string   `json:"message"  yaml:"message"  table:"MESSAGE"`
	Origin   Origin   `json:"origin"   yaml:"origin"   table:"ORIGIN"`
	Detail   string   `json:"detail"   yaml:"detail"   table:"DETAIL"`
}

// maxSocketPath is the portable bound on a Unix socket path, the same
// one kit's socket service refuses past.
const maxSocketPath = 103

// HTTPSurfaces are the services that listen on TCP: MCP unless it
// runs over stdio.
var HTTPSurfaces = []string{"api", "rpc", "mcp"}

// ListensOnTCP reports whether the HTTP surface svc, when it runs,
// listens on TCP: always, except MCP over stdio.
func ListensOnTCP(v *viper.Viper, svc string) bool {
	return svc != "mcp" || !strings.EqualFold(strings.TrimSpace(v.GetString("services.mcp.transport")), "stdio")
}

// AuthConfigured reports whether the HTTP surface svc has an
// authenticator: a kit mode (AuthModeConfigured), else the profile's
// surface token, named by credentials.surface_token_ref.
func AuthConfigured(v *viper.Viper, svc string) bool {
	return AuthModeConfigured(v, svc) || SurfaceTokenRef(v) != ""
}

// AuthModeConfigured reports whether the HTTP surface svc has a kit
// authenticator: services.<svc>.auth.mode, or the shared
// services.all.auth.mode every HTTP listener inherits. It takes
// precedence over the surface token.
func AuthModeConfigured(v *viper.Viper, svc string) bool {
	return strings.TrimSpace(v.GetString("services."+svc+".auth.mode")) != "" ||
		strings.TrimSpace(v.GetString("services.all.auth.mode")) != ""
}

// SurfaceTokenRef is the store key of the profile's surface token,
// empty when none is configured.
func SurfaceTokenRef(v *viper.Viper) string {
	return strings.TrimSpace(v.GetString(credentials.KeySurfaceTokenRef))
}

// Check is a validation a component that owns part of the
// configuration supplies, such as the storage driver registry. It
// reports each finding through report; the issue takes the origin of
// key.
type Check func(v *viper.Viper, report func(sev Severity, key, msg string))

// Validate checks the effective configuration without side effects:
// no file is written, nothing binds and no daemon is contacted. checks
// run after heo's own.
func Validate(v *viper.Viper, s *State, checks ...Check) []Issue {
	var issues []Issue
	// addAt reports key with the origin of from, the key whose value
	// caused the issue.
	addAt := func(sev Severity, key, from, msg string) {
		origin, detail := s.origin(from)
		issues = append(issues, Issue{Severity: sev, Key: key, Message: msg, Origin: origin, Detail: detail})
	}
	add := func(sev Severity, key, msg string) { addAt(sev, key, key, msg) }

	// The tenant is a uma principal token: uma's own rule decides.
	if err := model.TenantID(v.GetString(KeyTenant)).Validate(); err != nil {
		add(SeverityError, KeyTenant, err.Error())
	}

	if d, err := time.ParseDuration(v.GetString(KeyRetentionOperations)); err != nil || d <= 0 {
		add(SeverityError, KeyRetentionOperations, fmt.Sprintf(
			"%q: want a positive duration such as 720h", v.GetString(KeyRetentionOperations)))
	}

	if d, err := time.ParseDuration(v.GetString(KeyHeldExpiry)); err != nil || d <= 0 {
		add(SeverityError, KeyHeldExpiry, fmt.Sprintf(
			"%q: want a positive duration such as 72h", v.GetString(KeyHeldExpiry)))
	}
	if who := v.GetString(KeyHeldOnBehalfOf); who != "" {
		if err := model.ActorID(who).Validate(); err != nil {
			add(SeverityError, KeyHeldOnBehalfOf, err.Error())
		}
	}

	if _, err := trustcfg.Load(v); err != nil {
		add(SeverityError, trustcfg.Key, err.Error())
	}
	if _, err := approvechat.Load(v); err != nil {
		add(SeverityError, approvechat.Key, err.Error())
	}

	if v.GetBool("services.socket.enabled") {
		if p := v.GetString(KeySocketPath); len(p) > maxSocketPath {
			add(SeverityError, KeySocketPath, fmt.Sprintf(
				"%d bytes, over the %d-byte limit for a unix socket path; set a shorter %s",
				len(p), maxSocketPath, KeySocketPath))
		}
		if mode := v.GetString("services.socket.auth.mode"); mode != "peer" {
			add(SeverityWarning, "services.socket.auth.mode", fmt.Sprintf(
				"%q: without peer authentication the socket admits callers by file mode alone and no actor is verified", mode))
		}
		if !v.GetBool("services.socket.auth.peer.require_same_uid") {
			add(SeverityWarning, "services.socket.auth.peer.require_same_uid",
				"false: peers of other uids are admitted to the control socket")
		}
	}

	// A TCP surface is never served without an authenticator, loopback
	// included.
	for _, svc := range HTTPSurfaces {
		if !v.GetBool("services."+svc+".enabled") || !ListensOnTCP(v, svc) {
			continue
		}
		key := "services." + svc + ".auth.mode"
		if !AuthConfigured(v, svc) {
			addAt(SeverityError, key, "services."+svc+".enabled", fmt.Sprintf(
				"services.%s.enabled is true but no authenticator is configured; run `heo service setup %s`", svc, svc))
		}
	}

	validateCredentials(v, s, addAt)

	if v.GetBool("services.all.idempotency.enabled") {
		add(SeverityWarning, "services.all.idempotency.enabled",
			"true: kit's served replay ledger would answer repeated keys instead of uma's operation ledger")
	}

	for _, c := range checks {
		c(v, add)
	}

	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Severity != issues[j].Severity {
			return issues[i].Severity == SeverityError
		}
		return issues[i].Key < issues[j].Key
	})
	return issues
}

// validateCredentials checks the credentials.* section: a supported
// backend with its settings, no unknown key and no credential material.
func validateCredentials(v *viper.Viper, s *State, addAt func(sev Severity, key, from, msg string)) {
	known := map[string]bool{}
	for _, k := range credentials.Keys() {
		known[k] = true
	}
	for _, e := range s.Entries() {
		if !strings.HasPrefix(e.Key, "credentials.") || known[e.Key] {
			continue
		}
		if isSensitiveKey(e.Key) {
			addAt(SeverityError, e.Key, e.Key,
				"credential material never belongs in configuration: store it in the secret store and reference it")
			continue
		}
		addAt(SeverityError, e.Key, e.Key, fmt.Sprintf("unknown credentials key; known: %s",
			strings.Join(credentials.Keys(), ", ")))
	}
	for _, p := range credentials.ConfigFrom(v.GetString).Validate() {
		sev := SeverityError
		if p.Warning {
			sev = SeverityWarning
		}
		addAt(sev, p.Key, p.Key, p.Message)
	}
}

// HasErrors reports whether any issue is an error.
func HasErrors(issues []Issue) bool {
	for _, i := range issues {
		if i.Severity == SeverityError {
			return true
		}
	}
	return false
}
