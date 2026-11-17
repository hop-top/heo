package cmd

import (
	"testing"

	"github.com/spf13/cobra"

	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/cli/cmdmeta"
)

type schemaProbeA struct {
	Name string `json:"name"`
}

type schemaProbeB struct {
	Count int `json:"count"`
}

func annotations(c *cobra.Command) [2]string {
	return [2]string{c.Annotations[cmdmeta.KeyOutputSchema], c.Annotations[cmdmeta.KeyOutputSchemaVersion]}
}

// TestSetOutputSchemaMatchesKit: the cached annotations are the ones
// kit derives, on the first call and on every later one, and two types
// never share an entry.
func TestSetOutputSchemaMatchesKit(t *testing.T) {
	kit := func(zero any) [2]string {
		c := &cobra.Command{Use: "x"}
		if err := cli.SetOutputSchema(c, cli.OutputSchema{Type: zero, Version: "1.0"}); err != nil {
			t.Fatal(err)
		}
		return annotations(c)
	}
	wantA, wantB := kit(&schemaProbeA{}), kit(&schemaProbeB{})
	if wantA == wantB {
		t.Fatal("probe types must have different schemas")
	}
	for i := 0; i < 3; i++ {
		a, b := &cobra.Command{Use: "a"}, &cobra.Command{Use: "b"}
		setOutputSchema(a, &schemaProbeA{}, "1.0")
		setOutputSchema(b, &schemaProbeB{}, "1.0")
		if got := annotations(a); got != wantA {
			t.Errorf("call %d: type A annotations %q, want %q", i, got, wantA)
		}
		if got := annotations(b); got != wantB {
			t.Errorf("call %d: type B annotations %q, want %q", i, got, wantB)
		}
	}
	c := &cobra.Command{Use: "v"}
	setOutputSchema(c, &schemaProbeA{}, "2.1")
	if got := c.Annotations[cmdmeta.KeyOutputSchemaVersion]; got != "2.1" {
		t.Errorf("version %q, want 2.1: a cached entry must not leak across versions", got)
	}
}

// TestTreesBuiltTwiceCarrySameSchemas: a tree built from a warm cache
// declares exactly the schemas the first one did.
func TestTreesBuiltTwiceCarrySameSchemas(t *testing.T) {
	collect := func() map[string][2]string {
		out := map[string][2]string{}
		var walk func(c *cobra.Command)
		walk = func(c *cobra.Command) {
			if a := annotations(c); a[0] != "" {
				out[c.CommandPath()] = a
			}
			for _, s := range c.Commands() {
				walk(s)
			}
		}
		walk(newApp().r.Cmd)
		return out
	}
	first, second := collect(), collect()
	if len(first) == 0 {
		t.Fatal("no command declares an output schema")
	}
	if len(first) != len(second) {
		t.Fatalf("%d schemas, then %d", len(first), len(second))
	}
	for path, a := range first {
		if second[path] != a {
			t.Errorf("%s: schema differs between two trees", path)
		}
	}
}
