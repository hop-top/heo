package desk

import (
	"strings"
	"testing"

	"hop.top/uma/model"
)

// TestCheckStateLimits: a change is refused, UNSUPPORTED, when the limits
// a channel reports do not allow it: a flag kind the provider has not, a
// role it does not resolve, a label move on a provider that files a
// conversation in one folder; what they allow passes.
func TestCheckStateLimits(t *testing.T) {
	mail := model.StateLimits{Scope: model.ScopeAccount, Kinds: []string{model.FlagStar},
		Shape: model.MoveShapeLabels, Roles: []string{model.RoleInbox, model.RoleArchive}}
	folder := model.StateLimits{Scope: model.ScopeShared, Shape: model.MoveShapeFolder, Roles: []string{model.RoleInbox}}
	for name, tc := range map[string]struct {
		ch   StateChange
		l    model.StateLimits
		want string
	}{
		"star":                {StateChange{Verb: model.StateVerbFlag, Kind: model.FlagStar}, mail, ""},
		"unstar":              {StateChange{Verb: model.StateVerbUnflag, Kind: model.FlagStar}, mail, ""},
		"important":           {StateChange{Verb: model.StateVerbFlag, Kind: model.FlagImportant}, mail, "has no important flag; it has star"},
		"unimportant":         {StateChange{Verb: model.StateVerbUnflag, Kind: model.FlagImportant}, mail, "has no important flag"},
		"no kinds at all":     {StateChange{Verb: model.StateVerbFlag, Kind: model.FlagStar}, folder, "it has none"},
		"role resolved":       {StateChange{Verb: model.StateVerbMove, Destination: model.MoveDestination{Role: model.RoleArchive}}, mail, ""},
		"role not resolved":   {StateChange{Verb: model.StateVerbMove, Destination: model.MoveDestination{Role: model.RoleJunk}}, mail, "does not resolve the junk role; it resolves inbox, archive"},
		"id needs no role":    {StateChange{Verb: model.StateVerbMove, Destination: model.MoveDestination{ID: "X"}}, folder, ""},
		"labels on labels":    {StateChange{Verb: model.StateVerbMove, Add: []string{"a"}, Remove: []string{"b"}}, mail, ""},
		"labels on a folder":  {StateChange{Verb: model.StateVerbMove, Add: []string{"a"}}, folder, "files a conversation in one folder"},
		"remove on a folder":  {StateChange{Verb: model.StateVerbMove, Remove: []string{"a"}}, folder, "files a conversation in one folder"},
		"archive has no kind": {StateChange{Verb: model.StateVerbArchive}, mail, ""},
	} {
		err := checkStateLimits(StateVerbName(tc.ch.Verb), tc.ch, tc.l)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", name, err)
		case tc.want != "" && (!model.IsCode(err, model.Unsupported) || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want unsupported containing %q", name, err, tc.want)
		}
	}
}
