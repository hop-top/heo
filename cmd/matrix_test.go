package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/composition"
	"hop.top/heo/internal/matrix"
)

// matrixDoc is the page whose marked block is generated.
const matrixDoc = "docs/manual/provider-matrix.md"

// TestProviderMatrixIsCurrent: the block between the markers of the
// provider matrix is what the matrix package renders for this build's
// adapters. A new capability, a bumped maturity or a changed evidence
// entry fails here until `make docs-gen` regenerates the page.
func TestProviderMatrixIsCurrent(t *testing.T) {
	r := adapters.NewRegistry()
	if err := composition.Register(r, composition.Options{}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join("..", matrixDoc)
	cur, err := os.ReadFile(file) //nolint:gosec // G304: a fixed repository path
	if err != nil {
		t.Fatal(err)
	}
	want, err := matrix.Splice(string(cur), matrix.Render(r))
	if err != nil {
		t.Fatalf("%s: %v", matrixDoc, err)
	}
	if *updateDocs {
		if err := os.WriteFile(file, []byte(want), 0o600); err != nil { //nolint:gosec // G703: a fixed repository path
			t.Fatal(err)
		}
		return
	}
	if string(cur) != want {
		t.Errorf("%s is stale; run `make docs-gen` and commit the result", matrixDoc)
	}
}
