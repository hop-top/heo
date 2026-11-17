package xrrx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	xrr "hop.top/xrr"
)

func TestCheckRecordRefusesCI(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, ci := range []string{"true", "1", "TRUE", "yes"} {
		if err := CheckRecord(env(map[string]string{"CI": ci})); !errors.Is(err, ErrRecordInCI) {
			t.Errorf("CI=%q: %v, want ErrRecordInCI", ci, err)
		}
	}
	for _, ci := range []string{"", "0", "false", "FALSE"} {
		if err := CheckRecord(env(map[string]string{"CI": ci})); err != nil {
			t.Errorf("CI=%q: %v, want recording allowed", ci, err)
		}
	}
	for _, k := range []string{xrr.EnvRedactDisable, xrr.EnvRedactAllow} {
		if err := CheckRecord(env(map[string]string{k: "token"})); err == nil {
			t.Errorf("%s set: recording allowed", k)
		}
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]xrr.Mode{
		"": xrr.ModeReplay, "replay": xrr.ModeReplay, "record": xrr.ModeRecord, "passthrough": xrr.ModePassthrough,
	} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseMode("rec"); err == nil {
		t.Error("ParseMode accepted an unknown mode")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(xrr.EnvMode, "")
	if e, err := FromEnv(); e != nil || err != nil {
		t.Errorf("no XRR_MODE: %v, %v; want the process unrecorded", e, err)
	}

	dir := filepath.Join(t.TempDir(), "cassettes")
	t.Setenv(xrr.EnvMode, "replay")
	t.Setenv(xrr.EnvCassetteDir, dir)
	if _, err := FromEnv(); err == nil {
		t.Error("replay without a cassette directory was accepted")
	}

	// Record in CI is refused before anything is created.
	t.Setenv(xrr.EnvMode, "record")
	t.Setenv("CI", "true")
	if _, err := FromEnv(); !errors.Is(err, ErrRecordInCI) {
		t.Fatalf("record in CI: %v, want ErrRecordInCI", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("refused record created %s (%v)", dir, err)
	}

	t.Setenv("CI", "")
	e, err := FromEnv()
	if err != nil || e.Mode != xrr.ModeRecord || e.Dir != dir {
		t.Fatalf("record outside CI: %+v, %v", e, err)
	}
	t.Setenv(xrr.EnvMode, "replay")
	if e, err = FromEnv(); err != nil || e.Mode != xrr.ModeReplay {
		t.Fatalf("replay on the recorded directory: %+v, %v", e, err)
	}
}
