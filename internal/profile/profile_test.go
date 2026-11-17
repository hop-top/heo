package profile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/cite/scheme"
	"hop.top/kit/go/console/output"
)

// exitCode returns the kit exit code carried by err, or 0 for nil.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ce *output.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v (%T) carries no kit exit code", err, err)
	}
	return ce.ExitCode
}

func TestProfileRefGrammar(t *testing.T) {
	cases := []struct {
		in        string
		canonical string
		dir       string
		exit      int
		reason    string // substring of the usage error, when exit is 2
	}{
		// Defaults and native forms.
		{in: "", canonical: "heo://profile/default", dir: "default"},
		{in: "work", canonical: "heo://profile/work", dir: "work"},
		{in: "heo://profile/work", canonical: "heo://profile/work", dir: "work"},
		{in: "HEO://profile/work", canonical: "heo://profile/work", dir: "work"},
		{in: "a-0123456789abcd", canonical: "heo://profile/a-0123456789abcd", dir: "a-0123456789abcd"},
		// aps forms.
		{in: "aps:noor", canonical: "aps://profile/noor", dir: "aps-noor"},
		{in: "APS:noor", canonical: "aps://profile/noor", dir: "aps-noor"},
		{in: "aps://profile/noor", canonical: "aps://profile/noor", dir: "aps-noor"},
		{in: "Aps://profile/noor_2", canonical: "aps://profile/noor_2", dir: "aps-noor_2"},
		{in: "aps:0123456789abcdef", canonical: "aps://profile/0123456789abcdef", dir: "aps-0123456789abcdef"},
		// Usage errors.
		{in: "heo://other/work", exit: 2, reason: `namespace must be "profile"`},
		{in: "aps://agent/noor", exit: 2, reason: `namespace must be "profile"`},
		{in: "heo://profile/work/extra", exit: 2, reason: "single segment"},
		{in: "aps://profile/noor/extra", exit: 2, reason: "single segment"},
		{in: "aps://profile/no%2Fr", exit: 2, reason: "single segment"},
		{in: "aps://profile/no%2fr", exit: 2, reason: "single segment"},
		{in: "heo://profile/work?x=1", exit: 2, reason: "query and fragment"},
		{in: "heo://profile/work#frag", exit: 2, reason: "query and fragment"},
		{in: "aps:noor?x=1", exit: 2, reason: "query and fragment"},
		{in: "heo://profile", exit: 2},
		{in: "heo://profile/", exit: 2},
		{in: "aps:", exit: 2},
		{in: "Work", exit: 2, reason: "heo profile name must match"},
		{in: "1work", exit: 2},
		{in: "work_1", exit: 2},
		{in: "a-0123456789abcde", exit: 2}, // 17 bytes
		{in: "aps:Noor", exit: 2, reason: "aps profile id must match"},
		{in: "aps:0123456789abcdefg", exit: 2}, // 17 bytes
		{in: "aps:../x", exit: 2, reason: "single segment"},
		{in: "heo:work", exit: 2},
		{in: "foo://profile/x", exit: 2},
		{in: "wrk:x", exit: 2},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			ref, err := Parse(tc.in)
			if got := exitCode(t, err); got != tc.exit {
				t.Fatalf("Parse(%q) exit = %d (%v), want %d", tc.in, got, err, tc.exit)
			}
			if tc.exit != 0 {
				if tc.reason != "" && !strings.Contains(err.Error(), tc.reason) {
					t.Errorf("Parse(%q) = %q, want reason %q", tc.in, err, tc.reason)
				}
				return
			}
			if got := ref.String(); got != tc.canonical {
				t.Errorf("Parse(%q) = %s, want %s", tc.in, got, tc.canonical)
			}
			if got := ref.Dir(); got != tc.dir {
				t.Errorf("Parse(%q).Dir() = %s, want %s", tc.in, got, tc.dir)
			}
			// The canonical form parses back to itself.
			again, err := Parse(ref.String())
			if err != nil || again != ref {
				t.Errorf("Parse(%s) = %v, %v; want %v", ref, again, err, ref)
			}
		})
	}
}

func TestUnknownProfileSchemeListsSupported(t *testing.T) {
	for _, in := range []string{"foo://profile/x", "FOO://profile/x", "bar:x", "baz://profile/x"} {
		_, err := Parse(in)
		if got := exitCode(t, err); got != 2 {
			t.Fatalf("Parse(%q) exit = %d, want 2", in, got)
		}
		msg := err.Error()
		s := strings.ToLower(strings.SplitN(in, ":", 2)[0])
		if !strings.Contains(msg, `unsupported profile scheme "`+s+`"`) {
			t.Errorf("Parse(%q) = %q, want it to name scheme %q", in, msg, s)
		}
		if !strings.Contains(msg, "supported: aps, heo") {
			t.Errorf("Parse(%q) = %q, want it to list the supported schemes", in, msg)
		}
	}
	if got := strings.Join(Schemes(), ","); got != "aps,heo" {
		t.Errorf("Schemes() = %s, want aps,heo", got)
	}
}

func TestNativeNameRejectsForeignPrefix(t *testing.T) {
	for _, in := range []string{"aps-noor", "heo://profile/aps-noor", "aps-"} {
		_, err := Parse(in)
		if got := exitCode(t, err); got != 2 {
			t.Fatalf("Parse(%q) exit = %d (%v), want 2", in, got, err)
		}
		if !strings.Contains(err.Error(), `may not start with "aps-"`) {
			t.Errorf("Parse(%q) = %q, want the reserved-prefix reason", in, err)
		}
	}
	// The aps-bound form owns that directory name instead.
	ref, err := Parse("aps:noor")
	if err != nil || ref.Dir() != "aps-noor" {
		t.Fatalf("Parse(aps:noor) = %v (dir %q), %v", ref, ref.Dir(), err)
	}
}

// TestApsProfileURIMatchesAps pins heo's copy of aps's profile URI form:
// aps keeps ProfileURIScheme = "aps" and ProfileURISpace = "profile" in
// an internal package, and builds Profile.URI() as a cite URI from them.
func TestApsProfileURIMatchesAps(t *testing.T) {
	if ApsScheme != "aps" || ApsSpace != "profile" {
		t.Fatalf("aps constants drifted: scheme %q space %q", ApsScheme, ApsSpace)
	}
	if Namespace != ApsSpace {
		t.Fatalf("heo namespace %q differs from aps space %q", Namespace, ApsSpace)
	}
	apsURI := (&scheme.URI{Scheme: "aps", Namespace: "profile", ID: "noor"}).String()
	if apsURI != "aps://profile/noor" {
		t.Fatalf("aps canonical = %q", apsURI)
	}
	ref, err := Parse(apsURI)
	if err != nil {
		t.Fatalf("Parse(%s): %v", apsURI, err)
	}
	if ref.String() != apsURI {
		t.Errorf("heo canonical %s != aps canonical %s", ref, apsURI)
	}
}

func TestApsProfileMissingNotFound(t *testing.T) {
	data := t.TempDir()
	t.Setenv("APS_DATA_PATH", data)

	ref, err := Parse("aps:noor")
	if err != nil {
		t.Fatal(err)
	}
	err = CheckExists(ref)
	if got := exitCode(t, err); got != 3 {
		t.Fatalf("CheckExists(missing) exit = %d (%v), want 3", got, err)
	}
	if !strings.Contains(err.Error(), "aps://profile/noor") {
		t.Errorf("error %q does not name the reference", err)
	}

	// A directory without profile.yaml is not a profile either.
	if err := os.MkdirAll(filepath.Join(data, "profiles", "noor"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := exitCode(t, CheckExists(ref)); got != 3 {
		t.Fatalf("CheckExists(dir without profile.yaml) exit = %d, want 3", got)
	}

	if err := os.WriteFile(filepath.Join(data, "profiles", "noor", "profile.yaml"), []byte("id: noor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckExists(ref); err != nil {
		t.Fatalf("CheckExists(existing) = %v", err)
	}

	// Native profiles never consult aps.
	if err := CheckExists(Default()); err != nil {
		t.Fatalf("CheckExists(default) = %v", err)
	}
}

func TestApsDataDirFollowsAps(t *testing.T) {
	t.Setenv("APS_DATA_PATH", "")
	t.Setenv("XDG_DATA_HOME", "/xdg/data")
	got, err := ApsProfilePath("noor")
	if err != nil || got != "/xdg/data/aps/profiles/noor/profile.yaml" {
		t.Fatalf("ApsProfilePath with XDG_DATA_HOME = %q, %v", got, err)
	}
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/u")
	got, err = ApsProfilePath("noor")
	if err != nil || got != "/home/u/.local/share/aps/profiles/noor/profile.yaml" {
		t.Fatalf("ApsProfilePath default = %q, %v", got, err)
	}
	t.Setenv("APS_DATA_PATH", "/ops/aps")
	got, _ = ApsProfilePath("noor")
	if got != "/ops/aps/profiles/noor/profile.yaml" {
		t.Fatalf("ApsProfilePath with APS_DATA_PATH = %q", got)
	}
}
