package adapterstest

import (
	"context"
	"fmt"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// RawBytes is the copy DeclareRaw's default read returns for a provider
// message: an RFC 5322 message naming the provider's ID.
func RawBytes(providerID string) []byte {
	return []byte("From: ada@example.com\r\nSubject: " + providerID + "\r\nMessage-ID: <" + providerID + "@fake>\r\n\r\nraw body of " + providerID + "\r\n")
}

// DeclareRaw makes f declare raw_read. read answers a read of the
// provider message ref; nil answers RawBytes(ref.ID). An error is what the
// adapter reports for the read (a model.Error keeps its code and reason).
// Call it before the engine starts.
func (f *Factory) DeclareRaw(read func(ref model.ExternalRef) ([]byte, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if read == nil {
		read = func(ref model.ExternalRef) ([]byte, error) { return RawBytes(ref.ID), nil }
	}
	f.raw = read
}

// ReadRaw answers a raw read, after DeclareRaw (adapter.RawReader).
func (i *instance) ReadRaw(_ context.Context, r adapter.RawRequest) (adapter.RawMessage, error) {
	i.f.mu.Lock()
	read := i.f.raw
	i.f.mu.Unlock()
	if read == nil {
		return adapter.RawMessage{}, fmt.Errorf("fake declares no raw_read")
	}
	b, err := read(r.Message)
	if err != nil {
		return adapter.RawMessage{}, err
	}
	return adapter.RawMessage{MediaType: model.MediaTypeRawMessage, Bytes: b}, nil
}
