package desk

import (
	"strings"
	"testing"

	"hop.top/uma/model"
)

// TestCheckRecipientLimits: recipients are refused, UNSUPPORTED, when the
// limits a channel reports do not allow them: reply-all, cc or bcc it does
// not send, or more addresses than it carries (the route address counts as
// one); what they allow passes, and an unbounded channel carries any.
func TestCheckRecipientLimits(t *testing.T) {
	all := model.RecipientLimits{ReplyAll: true, Cc: true, Bcc: true, MaxRecipients: 4}
	ccOnly := model.RecipientLimits{Cc: true, MaxRecipients: 4}
	unbounded := model.RecipientLimits{ReplyAll: true, Cc: true, Bcc: true}
	addrs := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = string(rune('a'+i)) + "@example.com"
		}
		return out
	}
	for name, tc := range map[string]struct {
		r    model.ReplyRecipients
		l    model.RecipientLimits
		want string
	}{
		"reply-all":            {model.ReplyRecipients{ReplyAll: true}, all, ""},
		"reply-all refused":    {model.ReplyRecipients{ReplyAll: true}, ccOnly, "does not send reply-all"},
		"cc":                   {model.ReplyRecipients{Cc: addrs(1)}, ccOnly, ""},
		"cc refused":           {model.ReplyRecipients{Cc: addrs(1)}, model.RecipientLimits{ReplyAll: true}, "does not send cc"},
		"bcc refused":          {model.ReplyRecipients{Bcc: addrs(1)}, ccOnly, "does not send bcc"},
		"at the bound":         {model.ReplyRecipients{Cc: addrs(2), Bcc: addrs(1)}, all, ""},
		"over the bound":       {model.ReplyRecipients{Cc: addrs(2), Bcc: addrs(2)}, all, "5 recipients (the reply route's address, 2 cc, 2 bcc), this channel carries at most 4"},
		"unbounded":            {model.ReplyRecipients{Cc: addrs(20)}, unbounded, ""},
		"reply-all is no bulk": {model.ReplyRecipients{ReplyAll: true, Cc: addrs(3)}, all, ""},
	} {
		err := checkRecipientLimits("message reply", tc.r, tc.l)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", name, err)
		case tc.want != "" && (!model.IsCode(err, model.Unsupported) || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want unsupported containing %q", name, err, tc.want)
		}
	}
}
