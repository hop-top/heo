package cmd

import (
	"reflect"
	"sync"

	"github.com/spf13/cobra"

	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/cli/cmdmeta"
)

// schemaAnnotations is the output-schema annotation pair kit derives
// from one declared type.
type schemaAnnotations struct{ schema, version string }

// schemaCache holds the annotations by declared type and version. The
// schema is a function of the type alone, so reflecting and marshaling
// it once per process gives every command tree built after the first
// (one per CLI start, one per test invocation) the same bytes.
var schemaCache sync.Map // schemaKey -> schemaAnnotations

type schemaKey struct {
	typ     reflect.Type
	version string
}

// setOutputSchema is cli.SetOutputSchema for the declared type zero,
// reflecting it once per process: building the tree reflects and
// marshals about a hundred schemas, most of the cost of newApp.
func setOutputSchema(c *cobra.Command, zero any, version string) {
	key := schemaKey{reflect.TypeOf(zero), version}
	if v, ok := schemaCache.Load(key); ok {
		a := v.(schemaAnnotations)
		if c.Annotations == nil {
			c.Annotations = make(map[string]string)
		}
		c.Annotations[cmdmeta.KeyOutputSchema] = a.schema
		c.Annotations[cmdmeta.KeyOutputSchemaVersion] = a.version
		return
	}
	if err := cli.SetOutputSchema(c, cli.OutputSchema{Type: zero, Version: version}); err != nil {
		return
	}
	schemaCache.Store(key, schemaAnnotations{
		schema:  c.Annotations[cmdmeta.KeyOutputSchema],
		version: c.Annotations[cmdmeta.KeyOutputSchemaVersion],
	})
}
