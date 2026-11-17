package xrrx

import (
	"os"
	"testing"
)

// cassetteRoot is where the e2e tests keep their cassettes and fixtures.
const cassetteRoot = "../../tests/e2e/testdata"

// TestCommittedCassettesAreClean: every cassette in the repository
// passes the audit: no credential, host:port, temp path or process id,
// and no timestamp in a request. A re-recording that leaked fails here
// on the next plain `go test`, before review.
func TestCommittedCassettesAreClean(t *testing.T) {
	if _, err := os.Stat(cassetteRoot); os.IsNotExist(err) {
		t.Skip("no cassettes committed")
	}
	fs, err := Audit(cassetteRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		t.Errorf("%s", f)
	}
}
