package postgres

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"hop.top/heo/internal/storage"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

func TestInlinePassword(t *testing.T) {
	for _, tc := range []struct {
		dsn    string
		inline bool
		bad    bool
	}{
		{dsn: ""},
		{dsn: "host=db dbname=heo user=heo"},
		{dsn: "host=db password=x", inline: true},
		{dsn: "host=db password = 'a b\\' c'", inline: true},
		{dsn: "host=db options='-c search_path=heo'"},
		{dsn: "host=db application_name='password=x'"},
		{dsn: "postgres://heo@db/heo?sslmode=disable"},
		{dsn: "postgresql://heo:secret@db/heo", inline: true}, //nolint:gosec // G101: a placeholder DSN the redaction test must hide
		{dsn: "postgres://heo@db/heo?password=secret", inline: true},
		{dsn: "postgres://heo:@db/heo", inline: true},
		{dsn: "host=db dbname", bad: true},
		{dsn: "host='db", bad: true},
		{dsn: "postgres://heo:se cret@db:x/%zz", bad: true},
	} {
		inline, err := hasInlinePassword(tc.dsn)
		if (err != nil) != tc.bad || inline != tc.inline {
			t.Errorf("hasInlinePassword(%q) = %v, %v; want inline %v, error %v", tc.dsn, inline, err, tc.inline, tc.bad)
		}
		if err != nil && strings.Contains(err.Error(), "cret") {
			t.Errorf("error for %q echoes the password: %v", tc.dsn, err)
		}
	}
}

func settingsWith(kv ...string) storage.MapSettings {
	s := storage.MapSettings{}
	for k, v := range New(Options{}).Defaults() {
		s[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		s[kv[i]] = kv[i+1]
	}
	return s
}

func TestValidate(t *testing.T) {
	d := New(Options{})
	if probs := d.Validate(settingsWith()); len(probs) != 0 {
		t.Fatalf("defaults invalid: %+v", probs)
	}
	for _, tc := range []struct {
		s   storage.MapSettings
		key string
	}{
		{settingsWith(KeyDSN, "postgres://heo:pw@db/heo"), KeyDSN},
		{settingsWith(KeyDSN, "host=db port=notaport"), KeyDSN},
		{settingsWith(KeyPgDump, ""), KeyPgDump},
		{settingsWith(KeyPgRestore, ""), KeyPgRestore},
		{storage.MapSettings{KeyMaxConns: -1, KeyPgDump: "pg_dump", KeyPgRestore: "pg_restore"}, KeyMaxConns},
	} {
		probs := d.Validate(tc.s)
		if len(probs) != 1 || probs[0].Key != tc.key || probs[0].Warning {
			t.Errorf("Validate(%v) = %+v, want one error on %s", tc.s, probs, tc.key)
		}
	}
}

func TestConnectRefusesBadSettingsAndCredentials(t *testing.T) {
	ctx := context.Background()
	d := New(Options{})
	_, err := d.Connect(ctx, storage.Config{Settings: settingsWith(KeyDSN, "postgres://heo:pw@127.0.0.1:1/heo")})
	if model.CodeOf(err) != model.Validation || strings.Contains(err.Error(), "pw@") {
		t.Errorf("inline password: %v", err)
	}
	_, err = d.Connect(ctx, storage.Config{Settings: settingsWith(KeyDSN, "host=127.0.0.1 port=1", KeyPasswordRef, "ref")})
	if model.CodeOf(err) != model.Validation {
		t.Errorf("password_ref without a backend: %v", err)
	}
	var gotRef, gotPurpose string
	creds := storage.CredentialsFunc(func(_ context.Context, req storage.CredentialRequest) (ports.Credential, error) {
		gotRef, gotPurpose = req.Reference, req.Purpose
		return nil, errors.New("vault says: token s3cr3t expired")
	})
	_, err = d.Connect(ctx, storage.Config{
		Settings:    settingsWith(KeyDSN, "host=127.0.0.1 port=1", KeyPasswordRef, "heo/pg"),
		Credentials: creds,
	})
	if gotRef != "heo/pg" || gotPurpose != PasswordPurpose {
		t.Errorf("resolved %q for %q", gotRef, gotPurpose)
	}
	if model.CodeOf(err) != model.Unavailable || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("resolver failure: %v, want unavailable without the resolver's text", err)
	}
	creds = storage.CredentialsFunc(func(context.Context, storage.CredentialRequest) (ports.Credential, error) {
		return ports.OAuth2{AccessToken: "tok"}, nil
	})
	_, err = d.Connect(ctx, storage.Config{
		Settings:    settingsWith(KeyDSN, "host=127.0.0.1 port=1", KeyPasswordRef, "heo/pg"),
		Credentials: creds,
	})
	if model.CodeOf(err) != model.Unsupported {
		t.Errorf("oauth2 credential: %v, want unsupported", err)
	}
}

func TestResolvePasswordKinds(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		c          ports.Credential
		user, pass string
	}{
		{ports.Password{User: "app", Secret: []byte("p1")}, "app", "p1"},
		{ports.Secret{Bytes: []byte("p2")}, "", "p2"},
	} {
		var req storage.CredentialRequest
		creds := storage.CredentialsFunc(func(_ context.Context, r storage.CredentialRequest) (ports.Credential, error) {
			req = r
			return tc.c, nil
		})
		user, pass, err := resolvePassword(ctx, creds, "heo/pg")
		if err != nil || user != tc.user || string(pass) != tc.pass {
			t.Errorf("%T: %q %q %v", tc.c, user, pass, err)
		}
		if req.Reference != "heo/pg" || req.Purpose != PasswordPurpose ||
			len(req.Accept) != 2 || req.Accept[0] != ports.KindPassword || req.Accept[1] != ports.KindSecret {
			t.Errorf("request = %+v", req)
		}
	}
}

// TestExecRunnerPassesPasswordInEnv runs a stand-in pg_dump that prints
// its arguments and PGPASSWORD.
func TestExecRunnerPassesPasswordInEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in")
	}
	bin := filepath.Join(t.TempDir(), "pg_dump")
	script := "#!/bin/sh\necho \"args=$*\"\necho \"pw=$PGPASSWORD\"\necho oops >&2\nexit $EXIT\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test executable
		t.Fatal(err)
	}
	var out strings.Builder
	err := ExecRunner{}.Run(context.Background(), Command{
		Path: bin, Args: []string{"--dbname=x"}, Env: []string{"PGPASSWORD=pw1", "EXIT=0"}, Stdout: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "args=--dbname=x\npw=pw1\n" {
		t.Errorf("stand-in saw %q", out.String())
	}
	err = ExecRunner{}.Run(context.Background(), Command{Path: bin, Env: []string{"EXIT=3"}, Stdout: &out})
	if err == nil || !strings.Contains(err.Error(), "oops") {
		t.Errorf("failure = %v, want stderr in it", err)
	}
	err = toolError(context.Background(), "pg_dump",
		ExecRunner{}.Run(context.Background(), Command{Path: filepath.Join(t.TempDir(), "missing")}))
	if model.CodeOf(err) != model.Unavailable || !strings.Contains(err.Error(), "storage.postgres.pg_dump") {
		t.Errorf("missing tool = %v", err)
	}
}

func TestTablePattern(t *testing.T) {
	if got := tablePattern(`we"ird`); got != `"we""ird".uma_*` {
		t.Errorf("tablePattern = %s", got)
	}
}
