package cmd

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/cli/cmdmeta"
	"hop.top/kit/go/console/cli/service"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/console/output/envelope"
	"hop.top/kit/go/transport/cmdsurface"
)

// TestKitRegistersNoProfileFlag is a kit-bump tripwire: kit reserves
// --profile in a comment but registers none at the pin, so heo owns it
// through cli.Config.Globals. A kit release that registers it would
// make heo's registration a duplicate flag; on red, adopt kit's flag
// and keep heo's reference grammar.
func TestKitRegistersNoProfileFlag(t *testing.T) {
	bare := cli.New(cli.Config{Name: "heo", Version: "test", Short: "tripwire"},
		cli.WithStatus(cli.StatusConfig{}), service.WithSocket(service.SocketConfig{}))
	if f := bare.Cmd.PersistentFlags().Lookup(profileFlag); f != nil {
		t.Fatalf("kit now registers --%s itself (usage %q)", profileFlag, f.Usage)
	}
	a := newApp()
	if a.r.Cmd.PersistentFlags().Lookup(profileFlag) == nil {
		t.Fatalf("heo root lacks its --%s global", profileFlag)
	}
}

// TestExitEightNotKitAllocated is a kit-bump tripwire: heo publishes
// exit 8 for an outcome it cannot know (OUTCOME_UNKNOWN). Kit's band is
// 7 and 64-71 at the pin; a kit allocation of 8 would collide.
func TestExitEightNotKitAllocated(t *testing.T) {
	if class, ok := envelope.ClassForExitCode(8); ok {
		t.Fatalf("kit allocated exit 8 to %s", class)
	}
	for _, slot := range envelope.ExtensionBand() {
		if slot.Exit == 8 {
			t.Fatalf("kit extension band holds exit 8: %+v", slot)
		}
	}
}

func TestRootConventions(t *testing.T) {
	a := newApp()
	if a.r.Config.SignatureStrictness != cli.SignatureStrictnessReject {
		t.Errorf("SignatureStrictness = %q, want reject", a.r.Config.SignatureStrictness)
	}
	if err := a.r.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if rep := a.r.ValidateSignature(); len(rep.Violations) != 0 {
		t.Errorf("ValidateSignature: %+v", rep.Violations)
	}
	// No adopter flag shadows a kit global, and there is no --json.
	if a.r.Cmd.PersistentFlags().Lookup("json") != nil {
		t.Error("root registers --json; --format is the only output switch")
	}
}

// heoCommands returns heo's own command nodes: everything below the
// groups heo adds to the kit root.
func heoCommands(a *app) []*cobra.Command {
	var out []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		out = append(out, c)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	for _, c := range a.r.Cmd.Commands() {
		if slices.Contains([]string{"config", "service", "storage", "upgrade", "adapter", "channel", "inbox",
			"conversation", "label", "message", "note", "attachment", "contact", "identity", "delivery", "event",
			"reaction", "participant", "receipt", "history", "interaction", "task"}, c.Name()) {
			walk(c)
		}
	}
	return out
}

func TestEveryLeafAnnotatedAndGroupsNotRunnable(t *testing.T) {
	a := newApp()
	cmds := heoCommands(a)
	if len(cmds) == 0 {
		t.Fatal("no heo commands found")
	}
	for _, c := range cmds {
		path := c.CommandPath()
		if c.HasSubCommands() {
			if c.Runnable() {
				t.Errorf("%s: group is runnable", path)
			}
			continue
		}
		if _, ok := cli.GetSideEffect(c); !ok {
			t.Errorf("%s: no kit/side-effect", path)
		}
		if c.Annotations["kit/idempotent"] == "" {
			t.Errorf("%s: no kit/idempotent", path)
		}
		if strings.TrimSpace(c.Long) == "" {
			t.Errorf("%s: no Long", path)
		}
	}
	// One adopter verb at depth 1, upgrade, by declared exception: the
	// root's other children are kit's status and hidden, deprecated
	// serve, cobra builtins and the groups.
	for _, c := range a.r.Cmd.Commands() {
		if !c.HasSubCommands() && !slices.Contains([]string{"help", "status", "serve", "completion", "upgrade"}, c.Name()) {
			t.Errorf("depth-1 leaf %q", c.Name())
		}
	}
}

func child(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// TestServiceGroupIsKits: the service group is kit's, with heo's setup
// added and heo's start checks around kit's start and its hidden,
// deprecated serve spelling; root status is visible; the per-profile
// control scope and the own-config opt-out are on. ping is not in it:
// the socket withholds every leaf of kit's service group.
func TestServiceGroupIsKits(t *testing.T) {
	a := newApp()
	svc := service.ServiceCommand(a.r)
	if svc == nil || child(a.r.Cmd, "service") != svc || svc.Runnable() {
		t.Fatal("root service is not kit's non-runnable group")
	}
	for _, leaf := range []string{"start", "list", "stop", "restart", "status", "setup"} {
		if child(svc, leaf) == nil {
			t.Errorf("service %s missing", leaf)
		}
	}
	if child(svc, "ping") != nil {
		t.Error("service ping is under kit's service group, which the socket withholds")
	}
	start := service.ServeCommand(a.r)
	if child(svc, "start") != start || start.Annotations["kit/self-hosting"] != "true" {
		t.Fatal("service start is not kit's self-hosting start")
	}
	serve := rootServe(a.r)
	if serve == nil || !serve.Hidden {
		t.Fatal("root serve missing or visible")
	}
	if status := child(a.r.Cmd, "status"); status == nil || status.Hidden {
		t.Fatal("root status missing or hidden")
	}
	for _, c := range []*cobra.Command{start, serve} {
		if f := c.Flags().Lookup("insecure-remote"); f == nil {
			t.Fatalf("%s: kit no longer mounts --insecure-remote", c.CommandPath())
		}
		if err := c.Flags().Set("insecure-remote", "true"); err != nil {
			t.Fatal(err)
		}
		err := c.RunE(c, nil)
		var ce *output.Error
		if !errors.As(err, &ce) || ce.ExitCode != 2 || !strings.Contains(ce.Message, "--insecure-remote is refused") {
			t.Errorf("%s --insecure-remote: err = %v, want heo's refusal", c.CommandPath(), err)
		}
	}
	if err := a.r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestRefuseStartFlags(t *testing.T) {
	for _, name := range refusedStartFlags {
		c := &cobra.Command{Use: "start"}
		for _, f := range refusedStartFlags {
			c.Flags().Bool(f, false, "")
		}
		if err := refuseStartFlags(c); err != nil {
			t.Fatalf("unset flags refused: %v", err)
		}
		if err := c.Flags().Set(name, "true"); err != nil {
			t.Fatal(err)
		}
		var ce *output.Error
		if err := refuseStartFlags(c); !errors.As(err, &ce) || ce.ExitCode != 2 {
			t.Errorf("--%s: err = %v, want USAGE", name, err)
		}
	}
}

// TestDaemonPingIsSocketServed: `daemon ping` is heo's own command,
// mounted after cli.New, so kit's socket serves it: its group is no
// name kit reserved, no ancestor is a kit-reserved tree, and it
// withholds itself from REST, SSE, gRPC and MCP but not the socket.
// It is daemon-backed: the CLI sends it over the socket.
func TestDaemonPingIsSocketServed(t *testing.T) {
	a := newApp()
	grp := child(a.r.Cmd, "daemon")
	if grp == nil {
		t.Fatal("root daemon group missing")
	}
	ping := child(grp, "ping")
	if ping == nil || !ping.Runnable() {
		t.Fatal("daemon ping missing or not runnable")
	}
	if a.r.IsReserved("daemon") {
		t.Error("daemon is a kit-reserved name: the socket withholds it")
	}
	if cmdmeta.InReservedTree(ping) {
		t.Error("daemon ping is in a kit-reserved tree: the socket withholds it")
	}
	if !control.IsDaemon(ping) {
		t.Error("daemon ping is not daemon-backed")
	}
	got, err := cmdsurface.ParseWithholdFrom(ping.Annotations[cmdsurface.AnnotationWithholdFrom])
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []cmdsurface.Surface{cmdsurface.SurfaceREST, cmdsurface.SurfaceSSE, cmdsurface.SurfaceRPC, cmdsurface.SurfaceMCP} {
		if !slices.Contains(got, s) {
			t.Errorf("daemon ping served on %s", s)
		}
	}
	if slices.Contains(got, cmdsurface.SurfaceSocket) {
		t.Error("daemon ping withheld from the socket")
	}
}
