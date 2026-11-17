package hooks

import "io"

// StderrForTest exposes the stderr sink to the external tests.
func (l Launch) StderrForTest() io.Writer { return l.stderr() }
