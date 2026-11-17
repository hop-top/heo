package adapterstest

import (
	"errors"
	"strings"

	"hop.top/heo/internal/adapters"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// NormalizerType is the type key of the fake variant whose factory
// implements adapter.RecipientNormalizer (Normalizing), registered
// beside Type in the e2e build so a built binary can show what a start
// does to a recipient it spells differently than the provider does.
const NormalizerType model.TypeKey = "test.heo.fake.normalizer"

// Prefixes of the recipient IDs the normalizing variant refuses or
// answers wrongly, and the one it lower-cases: an address (an ID with an
// "@") is lower-cased, as an email address that ignores case is; every
// other ID is returned as given.
const (
	// RefusePrefix starts an ID the normalizer refuses with a safe
	// *model.Error: a start naming no identity the adapter can use.
	RefusePrefix = "refuse-"
	// RefuseUnsafePrefix starts an ID the normalizer refuses with a plain
	// error, whose text core never shows.
	RefuseUnsafePrefix = "refuse-unsafe-"
	// FaultKindPrefix starts an ID whose normalized recipient comes back
	// in another kind: an adapter fault, as core sees it.
	FaultKindPrefix = "fault-kind-"
	// FaultEmptyPrefix starts an ID whose normalized recipient comes
	// back with no ID: an invalid reference, an adapter fault.
	FaultEmptyPrefix = "fault-empty-"
)

// RefuseMessage is the safe message of the normalizer's refusal.
const RefuseMessage = "that handle cannot be reached"

// NormalizingFactory is the fake factory of the normalizing variant: a
// fake that also implements adapter.RecipientNormalizer.
type NormalizingFactory struct{ *Factory }

// Normalizing wraps f, which keeps its type, as a factory that
// implements adapter.RecipientNormalizer. The wrapper, not f, is what an
// engine registers.
func Normalizing(f *Factory) *NormalizingFactory { return &NormalizingFactory{Factory: f} }

// NormalizeRecipient lower-cases an address ID, refuses the IDs that
// start with RefusePrefix or RefuseUnsafePrefix, and answers the IDs
// that start with FaultKindPrefix or FaultEmptyPrefix with a recipient
// core must refuse as an adapter fault. It is pure.
func (*NormalizingFactory) NormalizeRecipient(r model.ExternalRef) (model.ExternalRef, error) {
	switch {
	case strings.HasPrefix(r.ID, RefuseUnsafePrefix):
		return model.ExternalRef{}, errors.New("backend at 10.0.0.7 said no")
	case strings.HasPrefix(r.ID, RefusePrefix):
		return model.ExternalRef{}, model.NewError(model.Validation, RefuseMessage)
	case strings.HasPrefix(r.ID, FaultKindPrefix):
		r.Kind = "other"
	case strings.HasPrefix(r.ID, FaultEmptyPrefix):
		r.ID = ""
	case strings.Contains(r.ID, "@"):
		r.ID = strings.ToLower(r.ID)
	}
	return r, nil
}

var _ adapter.RecipientNormalizer = (*NormalizingFactory)(nil)

// NormalizerAdapter returns the normalizing variant: the fake, under
// NormalizerType, over a factory that implements
// adapter.RecipientNormalizer. It is never in a release binary.
func NormalizerAdapter() adapters.Adapter {
	f := New()
	f.As(NormalizerType)
	a := f.Adapter()
	a.Factory = Normalizing(f)
	return a
}
