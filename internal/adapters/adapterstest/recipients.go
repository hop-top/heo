package adapterstest

import "hop.top/uma/model"

// RecipientsType declares reply_recipients with the limits of
// RecipientsLimits (reply-all and cc, no bcc, at most four addresses) and
// ships Gmail's reply draft, so a reply and a reply draft can carry
// recipients. A reply that carries any appends "reply <route> reply_all=
// cc= bcc=" to the effect log, a draft's effect its data as usual.
const RecipientsType model.TypeKey = "test.heo.fake.recipients"

// RecipientsLimits are the limits RecipientsType reports.
var RecipientsLimits = model.RecipientLimits{ReplyAll: true, Cc: true, Bcc: false, MaxRecipients: 4}

// DeclareRecipients makes f declare reply_recipients and report l as its
// limits, as a live mail adapter does: reply-all, cc and bcc as the
// limits allow. The fake records each reply's recipients on the request
// it receives (Sending, and the effect log's "reply" lines). Call it
// before the engine starts.
func (f *Factory) DeclareRecipients(l model.RecipientLimits) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recipients = &l
}
