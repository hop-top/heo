package authz

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"

	"hop.top/heo/internal/settings"
	"hop.top/uma/model"
)

// FromConfig builds the policy from the loaded configuration for
// tenant, with owner the uid the daemon runs as and service the
// runtime's own actor. A configuration with a blocking problem builds
// nothing: the daemon never runs on a policy it could only half read.
func FromConfig(v *viper.Viper, tenant model.TenantID, owner, service model.ActorID) (*Policy, error) {
	cfg, probs := read(v.Get, service)
	if err := problemsError(probs); err != nil {
		return nil, err
	}
	return New(cfg, tenant, owner, service), nil
}

// Check validates the policy offline, for `config validate` and
// `service start`: unknown roles and fields, invalid or reserved
// actors, and grants no caller can use — an actor no enabled surface
// can verify.
func Check(v *viper.Viper, owner, service model.ActorID) []Problem {
	cfg, probs := read(v.Get, service)
	reach := reachability(v)
	reported := map[model.ActorID]bool{}
	for i, g := range cfg.Grants {
		if g.Actor == owner || reach(g.Actor) || reported[g.Actor] {
			continue
		}
		reported[g.Actor] = true
		probs = append(probs, Problem{Key: fmt.Sprintf("%s[%d].actor", KeyGrants, grantIndex(v, g.Actor, i)), Warning: true,
			Message: fmt.Sprintf("%s cannot reach the daemon: the control socket admits only the daemon's own uid "+
				"(services.socket.auth.peer.require_same_uid) and no HTTP surface is enabled", g.Actor)})
	}
	return probs
}

// grantIndex is the position of actor's first grant in the configured
// list; fallback when the list cannot be read.
func grantIndex(v *viper.Viper, actor model.ActorID, fallback int) int {
	list, _ := v.Get(KeyGrants).([]any)
	for i, e := range list {
		if m, ok := e.(map[string]any); ok && m["actor"] == string(actor) {
			return i
		}
	}
	return fallback
}

// reachability reports which actors some enabled surface can verify:
// any token subject when an HTTP surface is enabled, any uid when the
// control socket admits peers of other uids.
func reachability(v *viper.Viper) func(model.ActorID) bool {
	http := false
	for _, svc := range settings.HTTPSurfaces {
		if v.GetBool("services."+svc+".enabled") && settings.ListensOnTCP(v, svc) {
			http = true
		}
	}
	otherUIDs := v.GetBool("services.socket.enabled") && !v.GetBool("services.socket.auth.peer.require_same_uid")
	return func(actor model.ActorID) bool {
		if IsPlugin(actor) {
			// Authenticated by its token on an HTTP surface only; the
			// socket never admits it.
			return http
		}
		return http || (otherUIDs && strings.HasPrefix(string(actor), "uid:"))
	}
}

// HasGrant reports whether the configured authorization.grants lists
// actor, whatever roles the entry holds.
func HasGrant(v *viper.Viper, actor model.ActorID) bool {
	list, _ := v.Get(KeyGrants).([]any)
	for _, e := range list {
		if m, ok := e.(map[string]any); ok && m["actor"] == string(actor) {
			return true
		}
	}
	return false
}
