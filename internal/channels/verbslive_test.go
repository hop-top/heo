package channels_test

import (
	"reflect"
	"slices"
	"testing"

	"hop.top/heo/internal/channels"
	"hop.top/uma/model"
)

// TestVerbsLiveCarriesTheStateLimits: a state verb whose capability is
// available shows the limits the adapter reported (scope, flag kinds, move
// shape and roles); an unavailable one, a verb of another capability and
// a report heo cannot read show none.
func TestVerbsLiveCarriesTheStateLimits(t *testing.T) {
	flag := model.StateLimits{Scope: model.ScopeAccount, Kinds: []string{model.FlagStar}}
	move := model.StateLimits{Scope: model.ScopeAccount, Shape: model.MoveShapeFolder, Roles: []string{model.RoleInbox, model.RoleJunk}}
	live := channels.VerbsLive([]model.CapabilityStatus{
		{Name: model.CapabilityFlag, Available: true, Limits: flag.Extension()},
		{Name: model.CapabilityMove, Available: true, Limits: move.Extension()},
		{Name: model.CapabilityArchive, Available: false, Reason: model.Unauthorized, Limits: model.StateLimits{Scope: model.ScopeShared}.Extension()},
		{Name: model.CapabilityTrash, Available: true, Limits: model.Extension{Name: "other", Version: 1}},
		{Name: model.CapabilityReadState, Available: true},
		{Name: model.CapabilityEdit, Available: true},
	})
	byVerb := map[string]channels.VerbLive{}
	for _, v := range live {
		byVerb[v.Verb] = v
	}
	for _, verb := range []string{"conversation flag", "conversation unflag"} {
		if l := byVerb[verb].Limits; l == nil || l.Scope != model.ScopeAccount || !slices.Equal(l.Kinds, []string{model.FlagStar}) {
			t.Errorf("%s limits = %+v", verb, l)
		}
	}
	if l := byVerb["conversation move"].Limits; l == nil || l.Shape != model.MoveShapeFolder || !slices.Equal(l.Roles, move.Roles) {
		t.Errorf("conversation move limits = %+v", l)
	}
	for _, verb := range []string{"conversation archive", "conversation trash", "conversation read", "message edit", "message delete"} {
		if l := byVerb[verb].Limits; l != nil {
			t.Errorf("%s shows limits %+v", verb, l)
		}
	}
	if st := byVerb["conversation archive"]; st.State != "unauthorized" || st.Code != "PROVIDER_PERMISSION" {
		t.Errorf("conversation archive = %+v", st)
	}
}

// TestRecipientsLiveCarriesTheLimits: the reply recipient flags show the
// state of reply_recipients and, while it is available, the limits the
// adapter reported; unavailable, unreported or unreadable, they show
// none, and the state says why.
func TestRecipientsLiveCarriesTheLimits(t *testing.T) {
	l := model.RecipientLimits{ReplyAll: true, Cc: true, MaxRecipients: 51}
	for name, tc := range map[string]struct {
		st     []model.CapabilityStatus
		state  string
		limits *model.RecipientLimits
		code   string
	}{
		"available":    {[]model.CapabilityStatus{{Name: model.CapabilityReplyRecipients, Available: true, Limits: l.Extension()}}, "available", &l, ""},
		"unreadable":   {[]model.CapabilityStatus{{Name: model.CapabilityReplyRecipients, Available: true, Limits: model.Extension{Name: "other", Version: 1}}}, "available", nil, ""},
		"unauthorized": {[]model.CapabilityStatus{{Name: model.CapabilityReplyRecipients, Reason: model.Unauthorized, Limits: l.Extension()}}, "unauthorized", nil, "PROVIDER_PERMISSION"},
		"not reported": {[]model.CapabilityStatus{{Name: model.CapabilityEdit, Available: true}}, "unsupported", nil, "UNSUPPORTED"},
	} {
		got := channels.RecipientsLive(tc.st)
		if got.State != tc.state || got.Code != tc.code || got.Capability != model.CapabilityReplyRecipients || !reflect.DeepEqual(got.Recipients, tc.limits) || got.Limits != nil {
			t.Errorf("%s: %+v, want state %s, code %q, limits %+v", name, got, tc.state, tc.code, tc.limits)
		}
	}
}
