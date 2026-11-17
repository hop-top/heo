package xrrx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	xrr "hop.top/xrr"

	"hop.top/heo/internal/storage/postgres"
)

// tool is a Runner standing in for pg_dump and pg_restore: it counts its
// runs and writes out, to the command's stdout, and fails when fail is
// set.
type tool struct {
	runs  int
	out   []byte
	fail  error
	stdin []byte
}

func (f *tool) Run(_ context.Context, c postgres.Command) error {
	f.runs++
	if c.Stdin != nil {
		f.stdin, _ = io.ReadAll(c.Stdin)
	}
	if c.Stdout != nil {
		_, _ = c.Stdout.Write(f.out)
	}
	return f.fail
}

func dump(dsn string) postgres.Command {
	return postgres.Command{
		Path: "/opt/pg/bin/pg_dump",
		Args: []string{"--format=custom", "--no-password", "--table=public.*", "--dbname=" + dsn},
		Env:  []string{"PGPASSWORD=hunter2-not-for-disk"},
	}
}

func runner(mode xrr.Mode, dir string, inner postgres.Runner) postgres.Runner {
	return (&Env{Mode: mode, Dir: dir, MissLog: filepath.Join(dir, "..", "miss.log")}).Runner(inner)
}

// TestRunnerRecordReplay: record runs the tool once and keeps what it
// wrote; replay writes the same bytes without starting it, under another
// connection string, and keeps no password, path or host.
func TestRunnerRecordReplay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cas")
	archive := []byte("PGDMP\x00\xff\xfe binary \x80 archive")
	rec := &tool{out: archive}
	c := dump("postgres://heo@127.0.0.1:55001/heo_e2e_4?sslmode=disable")
	var got bytes.Buffer
	c.Stdout = &got
	if err := runner(xrr.ModeRecord, dir, rec).Run(t.Context(), c); err != nil || rec.runs != 1 || !bytes.Equal(got.Bytes(), archive) {
		t.Fatalf("record: err %v, runs %d, stdout %q", err, rec.runs, got.Bytes())
	}
	all := readAll(t, dir)
	for _, bad := range []string{"hunter2", "PGPASSWORD", "55001", "127.0.0.1", "heo_e2e_4", "/opt/pg"} {
		if strings.Contains(all, bad) {
			t.Errorf("cassette keeps %q:\n%s", bad, all)
		}
	}
	if fs, err := Audit(dir); err != nil || len(fs) != 0 {
		t.Errorf("audit = %v, %v", fs, err)
	}

	again := &tool{out: []byte("never")}
	c = dump("postgres://heo@127.0.0.1:61234/heo_e2e_9?sslmode=disable")
	got.Reset()
	c.Stdout = &got
	if err := runner(xrr.ModeReplay, dir, again).Run(t.Context(), c); err != nil || again.runs != 0 || !bytes.Equal(got.Bytes(), archive) {
		t.Fatalf("replay: err %v, runs %d, stdout %q; want the recorded bytes and no run", err, again.runs, got.Bytes())
	}
}

// TestRunnerReplayMissIsStrict: a command with no cassette fails and is
// logged; the tool does not start. Different stdin is a different
// command.
func TestRunnerReplayMissIsStrict(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cas")
	rec := &tool{}
	restore := func(stdin string) postgres.Command {
		return postgres.Command{Path: "pg_restore", Args: []string{"--no-password", "--dbname=postgres://x/y"}, Stdin: strings.NewReader(stdin)}
	}
	if err := runner(xrr.ModeRecord, dir, rec).Run(t.Context(), restore("archive one")); err != nil || string(rec.stdin) != "archive one" {
		t.Fatalf("record: %v, stdin %q", err, rec.stdin)
	}
	never := &tool{}
	err := runner(xrr.ModeReplay, dir, never).Run(t.Context(), restore("archive two"))
	if !errors.Is(err, xrr.ErrCassetteMiss) || never.runs != 0 {
		t.Fatalf("replay of another stdin = %v, runs %d; want a cassette miss and no run", err, never.runs)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "..", "miss.log")) //nolint:gosec // G304: the test's own miss log
	if !strings.Contains(string(log), "no cassette for") {
		t.Errorf("miss log = %q", log)
	}
	if err := runner(xrr.ModeReplay, dir, never).Run(t.Context(), restore("archive one")); err != nil || never.runs != 0 {
		t.Errorf("replay of the recorded stdin = %v, runs %d", err, never.runs)
	}
}

// TestRunnerReplaysFailure: a failing run replays as a failure, its
// message intact.
func TestRunnerReplaysFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cas")
	c := postgres.Command{Path: "pg_dump", Args: []string{"--no-password"}}
	if err := runner(xrr.ModeRecord, dir, &tool{fail: errors.New("pg_dump: exit status 1: no such table")}).Run(t.Context(), c); err == nil {
		t.Fatal("record swallowed the failure")
	}
	never := &tool{}
	err := runner(xrr.ModeReplay, dir, never).Run(t.Context(), c)
	if err == nil || !strings.Contains(err.Error(), "no such table") || never.runs != 0 {
		t.Errorf("replay = %v, runs %d; want the recorded failure", err, never.runs)
	}
}

// TestRunnerRefusesRedactedStdout: xrr replaces a stdout holding a
// credential shape with a placeholder; replaying that as the tool's
// output would corrupt what the code under test writes, so it fails.
func TestRunnerRefusesRedactedStdout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cas")
	var sink bytes.Buffer
	c := postgres.Command{Path: "pg_dump", Args: []string{"--no-password"}, Stdout: &sink}
	rec := &tool{out: []byte("-- token ghp_abcdefghijklmnopqrstuvwxyz0123456789\n")}
	if err := runner(xrr.ModeRecord, dir, rec).Run(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readAll(t, dir), "ghp_") {
		t.Fatal("the token reached the cassette")
	}
	sink.Reset()
	err := runner(xrr.ModeReplay, dir, &tool{}).Run(t.Context(), c)
	if err == nil || !strings.Contains(err.Error(), "redacted") || sink.Len() != 0 {
		t.Errorf("replay = %v, wrote %q; want a refusal that writes nothing", err, sink.String())
	}
}
