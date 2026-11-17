package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"hop.top/kit/go/console/output"
)

func TestSetupInputs(t *testing.T) {
	in, err := parseSetupInputs([]string{"mcp", "api", "mcp"}, []string{"api=127.0.0.1:9000", "mcp=[::1]:9001"},
		[]string{"rpc"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in.enable, []string{"api", "mcp"}) || !reflect.DeepEqual(in.disable, []string{"rpc"}) ||
		in.addrs["api"] != "127.0.0.1:9000" || in.addrs["mcp"] != "[::1]:9001" || !in.rotate || in.show || in.empty() {
		t.Errorf("inputs = %+v", in)
	}
	if in, _ := parseSetupInputs(nil, nil, nil, false, false); !in.empty() {
		t.Errorf("no arguments is not empty: %+v", in)
	}

	for name, c := range map[string]struct {
		args, addrs, disable []string
		want                 string
	}{
		"unknown surface":     {args: []string{"socket"}, want: `unknown surface "socket"`},
		"unknown disable":     {disable: []string{"web"}, want: `unknown surface "web"`},
		"enable and disable":  {args: []string{"api"}, disable: []string{"api"}, want: "both enabled and disabled"},
		"addr without =":      {addrs: []string{"127.0.0.1:1"}, want: "want <surface>=<host:port>"},
		"addr unknown":        {addrs: []string{"web=127.0.0.1:1"}, want: `unknown surface "web"`},
		"addr of disabled":    {addrs: []string{"api=127.0.0.1:1"}, disable: []string{"api"}, want: "being disabled"},
		"addr all interfaces": {args: []string{"api"}, addrs: []string{"api=0.0.0.0:8080"}, want: "not a loopback address"},
		"addr unspecified v6": {args: []string{"api"}, addrs: []string{"api=[::]:8080"}, want: "not a loopback address"},
		"addr lan":            {args: []string{"api"}, addrs: []string{"api=192.168.1.10:8080"}, want: "not a loopback address"},
		"addr empty host":     {args: []string{"api"}, addrs: []string{"api=:8080"}, want: "not a loopback address"},
		"addr host name":      {args: []string{"api"}, addrs: []string{"api=localhost:8080"}, want: "not a loopback address"},
		"addr port zero":      {args: []string{"api"}, addrs: []string{"api=127.0.0.1:0"}, want: "1-65535"},
		"addr no port":        {args: []string{"api"}, addrs: []string{"api=127.0.0.1"}, want: "host:port"},
	} {
		_, err := parseSetupInputs(c.args, c.addrs, c.disable, false, false)
		var e *output.Error
		if !errors.As(err, &e) || e.ExitCode != output.ExitUsage || !strings.Contains(e.Message, c.want) {
			t.Errorf("%s: err = %v, want USAGE containing %q", name, err, c.want)
		}
	}
}

// TestSetupProposesNextFreePort: a configured loopback address is kept;
// otherwise kit's default port, or the next one free and not taken by
// another surface.
func TestSetupProposesNextFreePort(t *testing.T) {
	busy := map[string]bool{"127.0.0.1:8080": true, "127.0.0.1:8081": true}
	free := func(addr string) bool { return !busy[addr] }

	got := proposeAddrs(map[string]string{}, map[string]string{}, free)
	want := map[string]string{"api": "127.0.0.1:8082", "rpc": "127.0.0.1:8083", "mcp": "127.0.0.1:8084"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("proposals = %v, want %v", got, want)
	}

	got = proposeAddrs(map[string]string{"api": "127.0.0.1:8080", "rpc": "0.0.0.0:8082"},
		map[string]string{"mcp": "127.0.0.1:8083"}, free)
	want = map[string]string{"api": "127.0.0.1:8080", "rpc": "127.0.0.1:8082", "mcp": "127.0.0.1:8084"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("proposals with configuration = %v, want %v", got, want)
	}

	got = proposeAddrs(map[string]string{}, map[string]string{}, func(string) bool { return true })
	want = map[string]string{"api": "127.0.0.1:8080", "rpc": "127.0.0.1:8082", "mcp": "127.0.0.1:8081"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("default proposals = %v, want %v", got, want)
	}
}

// TestSetupWizardAsksOnlyMissing: on a terminal the wizard asks for
// the surfaces when nothing was asked, then each chosen surface's
// address without --addr, re-asking past an address that is not
// loopback; it asks nothing when nothing is missing.
func TestSetupWizardAsksOnlyMissing(t *testing.T) {
	ctx := context.Background()
	defaults := map[string]string{"api": "127.0.0.1:8080", "rpc": "127.0.0.1:8082", "mcp": "127.0.0.1:8081"}
	empty := setupInputs{addrs: map[string]string{}}

	// Surfaces 1 and 3 (api, mcp); api takes its default, mcp is first
	// given a LAN address, refused, then a loopback one.
	var out bytes.Buffer
	in, err := promptSetup(ctx, empty, nil, defaults,
		strings.NewReader("1,3\n\n10.0.0.1:9\n127.0.0.1:9001\n"), &out)
	if err != nil {
		t.Fatal(err, out.String())
	}
	if !reflect.DeepEqual(in.enable, []string{"api", "mcp"}) || in.addrs["api"] != "127.0.0.1:8080" ||
		in.addrs["mcp"] != "127.0.0.1:9001" || in.addrs["rpc"] != "" {
		t.Errorf("inputs = %+v\n%s", in, out.String())
	}
	for _, want := range []string{"Surfaces to enable", "api listen address (loopback) [127.0.0.1:8080]", "not a loopback address"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("prompts lack %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "rpc listen address") {
		t.Errorf("asked for an unchosen surface's address:\n%s", out.String())
	}

	// Surfaces named, one address given: only the other is asked.
	out.Reset()
	named := setupInputs{enable: []string{"api", "rpc"}, addrs: map[string]string{"api": "127.0.0.1:7000"}}
	in, err = promptSetup(ctx, named, nil, defaults, strings.NewReader("\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Surfaces") || strings.Contains(out.String(), "api listen") ||
		!strings.Contains(out.String(), "rpc listen address") || in.addrs["rpc"] != "127.0.0.1:8082" {
		t.Errorf("named surfaces: inputs %+v, prompts:\n%s", in, out.String())
	}

	// Nothing missing: nothing asked, nothing read.
	out.Reset()
	full := setupInputs{enable: []string{"api"}, addrs: map[string]string{"api": "127.0.0.1:7000"}}
	if _, err := promptSetup(ctx, full, nil, defaults, strings.NewReader("garbage\n"), &out); err != nil || out.Len() != 0 {
		t.Errorf("complete inputs prompted: %v\n%s", err, out.String())
	}
	rotateOnly := setupInputs{rotate: true, addrs: map[string]string{}}
	if _, err := promptSetup(ctx, rotateOnly, []string{"api"}, defaults, strings.NewReader(""), &out); err != nil || out.Len() != 0 {
		t.Errorf("--rotate-token alone prompted: %v\n%s", err, out.String())
	}

	// End of input aborts with USAGE, nothing chosen.
	_, err = promptSetup(ctx, empty, nil, defaults, strings.NewReader(""), &out)
	var e *output.Error
	if !errors.As(err, &e) || e.ExitCode != output.ExitUsage || !strings.Contains(e.Message, "aborted") {
		t.Errorf("EOF = %v", err)
	}
}

func TestSurfaceBearerToken(t *testing.T) {
	for header, want := range map[string]string{
		"Bearer abc":   "abc",
		"bearer  abc ": "abc",
		"BEARER abc":   "abc",
		"Basic abc":    "",
		"Bearer":       "",
		"Bearer ":      "",
		"abc":          "",
		"":             "",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		got, ok := bearerToken(r)
		if got != want || ok != (want != "") {
			t.Errorf("%q = %q, %v; want %q", header, got, ok, want)
		}
	}
}
