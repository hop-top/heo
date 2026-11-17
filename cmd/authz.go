package cmd

import (
	"sync"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/settings"
	"hop.top/uma/model"
)

// policyState is the profile's authorization policy, built once from
// the loaded configuration: the control gate admits callers by it and
// the runtime's engine decides every uma operation by it, so every
// surface, and every worker's recheck, decides alike.
type policyState struct {
	once   sync.Once
	policy *authz.Policy
	err    error
}

// authorization returns the profile's policy. The daemon's own uid is
// the installation owner.
func (a *app) authorization() (*authz.Policy, error) {
	a.authz.once.Do(func() {
		v := a.r.Viper
		a.authz.policy, a.authz.err = authz.FromConfig(v,
			model.TenantID(v.GetString(settings.KeyTenant)), authz.OwnerActor(), daemon.ServiceActor)
	})
	return a.authz.policy, a.authz.err
}

// admit is the control gate's admitter: a verified actor reaches the
// daemon's commands only when the policy grants it a role. A policy
// that does not read admits no one.
func (a *app) admit(actor model.ActorID) (bool, string) {
	p, err := a.authorization()
	if err != nil {
		return false, reasonPolicyInvalid
	}
	return p.Admits(actor)
}

// reasonPolicyInvalid refuses every caller of a daemon whose policy
// does not read; `service start` refuses such a configuration first.
const reasonPolicyInvalid = "heo.authz.policy_invalid"
