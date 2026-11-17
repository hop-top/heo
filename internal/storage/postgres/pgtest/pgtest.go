// Package pgtest runs a throwaway PostgreSQL server for tests of the
// postgres storage driver and of the commands built on it.
//
// One container per test binary serves every test; each store is its
// own database in it. pg_dump and pg_restore run inside that container
// (Runner), so the client tools always match the server and the host
// needs none installed.
//
// A missing container runtime skips the tests, unless HEO_REQUIRE_DOCKER
// is 1: then it fails them.
package pgtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"hop.top/heo/internal/storage"
	"hop.top/heo/internal/storage/postgres"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	umapg "hop.top/uma/store/postgres"
)

// Image is the server the tests run against, the one uma's store tests
// pin, by multi-platform index digest. Its pg_dump and pg_restore run
// the backups, so client and server versions always match.
const Image = "postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15"

// RequireDockerEnv makes a missing container runtime fail the tests
// instead of skipping them.
const RequireDockerEnv = "HEO_REQUIRE_DOCKER"

// Login the stores are reached with.
const (
	User     = "heo"
	Password = "conformance-secret-7f3a" //nolint:gosec // G101: throwaway test server password
	// PasswordRef is the credential reference Credentials resolves.
	PasswordRef = "test://storage/postgres/password"
)

// Server is the shared test server.
type Server struct {
	ctr   *tcpostgres.PostgresContainer
	admin *pgxpool.Pool
	host  string // host:port the host reaches the server on
	next  atomic.Int64
}

var shared struct {
	once sync.Once
	srv  *Server
	err  error
}

// Require returns the shared server, starting it on first use. Call
// Terminate from TestMain after m.Run.
func Require(t *testing.T) *Server {
	t.Helper()
	if os.Getenv(RequireDockerEnv) != "1" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
	}
	shared.once.Do(func() { shared.srv, shared.err = start() })
	if shared.err != nil {
		t.Fatalf("postgres test server: %v", shared.err)
	}
	return shared.srv
}

// Terminate stops the shared server if it was started.
func Terminate() error {
	s := shared.srv
	if s == nil {
		return nil
	}
	if s.admin != nil {
		s.admin.Close()
	}
	if s.ctr != nil {
		return testcontainers.TerminateContainer(s.ctr)
	}
	return nil
}

func start() (*Server, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername(User),
		tcpostgres.WithPassword(Password),
		tcpostgres.BasicWaitStrategies(),
	)
	s := &Server{ctr: ctr}
	if err != nil {
		return s, fmt.Errorf("start postgres container: %w", err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return s, err
	}
	if s.admin, err = pgxpool.New(ctx, dsn); err != nil {
		return s, err
	}
	cfg := s.admin.Config().ConnConfig
	s.host = fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	return s, nil
}

// ContainerID names the server's container, for tools run with
// `docker exec`.
func (s *Server) ContainerID() string { return s.ctr.GetContainerID() }

// Host is the host:port the host reaches the server on.
func (s *Server) Host() string { return s.host }

// Runner runs pg_dump and pg_restore inside the test server's
// container, where the client tools match the server. It reaches the
// server on the container's own port and keeps every command it ran.
type Runner struct {
	s    *Server
	mu   sync.Mutex
	runs []postgres.Command
	n    atomic.Int64
}

// Runner returns a new Runner on s.
func (s *Server) Runner() *Runner { return &Runner{s: s} }

// Run runs c in the container.
func (r *Runner) Run(ctx context.Context, c postgres.Command) error {
	r.mu.Lock()
	r.runs = append(r.runs, c)
	r.mu.Unlock()
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = strings.Replace(a, "@"+r.s.host+"/", "@localhost:5432/", 1)
	}
	n := r.n.Add(1)
	in, out := fmt.Sprintf("/tmp/heo-run-%p-%d.in", r, n), fmt.Sprintf("/tmp/heo-run-%p-%d.out", r, n)
	script := `"$0" "$@"`
	if c.Stdin != nil {
		data, err := io.ReadAll(c.Stdin)
		if err != nil {
			return err
		}
		if err := r.s.ctr.CopyToContainer(ctx, data, in, 0o644); err != nil {
			return err
		}
		script += " < " + in
	}
	if c.Stdout != nil {
		script += " > " + out
	}
	code, rd, err := r.s.ctr.Exec(ctx, append([]string{"sh", "-c", script, c.Path}, args...),
		tcexec.WithEnv(c.Env), tcexec.Multiplexed())
	if err != nil {
		return err
	}
	msg, _ := io.ReadAll(rd)
	if code != 0 {
		return fmt.Errorf("%s exit %d: %s", c.Path, code, bytes.TrimSpace(msg))
	}
	if c.Stdout != nil {
		rc, err := r.s.ctr.CopyFileFromContainer(ctx, out)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		if _, err := io.Copy(c.Stdout, rc); err != nil {
			return err
		}
	}
	return nil
}

// Commands returns every command the runner ran.
func (r *Runner) Commands() []postgres.Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]postgres.Command(nil), r.runs...)
}

// Credentials resolves only PasswordRef, for the password purpose.
var Credentials = storage.CredentialsFunc(func(_ context.Context, req storage.CredentialRequest) (ports.Credential, error) {
	if req.Reference != PasswordRef || req.Purpose != postgres.PasswordPurpose || !slices.Contains(req.Accept, ports.KindPassword) {
		return nil, model.NewError(model.NotFound, "unexpected credential request")
	}
	return ports.Password{User: User, Secret: []byte(Password)}, nil
})

// NewDatabase creates an empty database no other test shares, dropped
// when t ends, and returns its DSN (without a password).
func (s *Server) NewDatabase(t testing.TB) string {
	t.Helper()
	name := fmt.Sprintf("heo_test_%d_%d", os.Getpid(), s.next.Add(1))
	if _, err := s.admin.Exec(context.Background(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		drop := "DROP DATABASE " + pgx.Identifier{name}.Sanitize() + " WITH (FORCE)"
		if _, err := s.admin.Exec(context.Background(), drop); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})
	return fmt.Sprintf("postgres://%s@%s/%s?sslmode=disable", User, s.host, name)
}

// NewStore provisions a database and returns the driver configuration
// that reaches it, with a fresh backup directory.
func (s *Server) NewStore(t testing.TB) storage.Config {
	t.Helper()
	return storage.Config{
		Settings: storage.MapSettings{
			postgres.KeyDSN:         s.NewDatabase(t),
			postgres.KeyPasswordRef: PasswordRef,
			postgres.KeyPgDump:      "pg_dump",
			postgres.KeyPgRestore:   "pg_restore",
		},
		Credentials: Credentials,
		BackupDir:   filepath.Join(t.TempDir(), "backups"),
		Owner:       "heo/conformance",
	}
}

// Database names the database a DSN from NewDatabase points at.
func Database(dsn string) string {
	return strings.TrimSuffix(dsn[strings.LastIndex(dsn, "/")+1:], "?sslmode=disable")
}

// Pool opens an admin-credentialed pool on the database dsn names.
func (s *Server) Pool(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	pc := s.admin.Config().Copy()
	pc.ConnConfig.Database = Database(dsn)
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// umaMigrations is the migration directory of the uma store module this
// binary builds against. It is looked up when the package loads, before a
// test points HOME elsewhere: from a home without the module cache,
// `go list` answers an empty directory for a published module.
var umaMigrations = func() func() (string, error) {
	dir, err := umaMigrationsDir()
	return func() (string, error) { return dir, err }
}()

func umaMigrationsDir() (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "hop.top/uma/store/postgres").Output()
	if err != nil {
		return "", fmt.Errorf("go list uma store module: %w", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", errors.New("go list uma store module: no directory: module not downloaded")
	}
	return filepath.Join(dir, "migrations"), nil
}

// Downgrade rolls the newest uma migration back with its own Down
// section, as the previous release left the schema.
func (s *Server) Downgrade(t testing.TB, dsn string) {
	t.Helper()
	dir, err := umaMigrations()
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(s.Pool(t, dsn))
	defer func() { _ = db.Close() }()
	p, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(dir),
		goose.WithDisableGlobalRegistry(true), goose.WithTableName(umapg.VersionTable))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(context.Background(), umapg.SchemaVersion-1); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
}

// MarkNewer records one migration past the library's as applied: the
// store as a newer heo would have left it.
func (s *Server) MarkNewer(t testing.TB, dsn string) {
	t.Helper()
	st, err := database.NewStore(database.DialectPostgres, umapg.VersionTable)
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(s.Pool(t, dsn))
	defer func() { _ = db.Close() }()
	if err := st.Insert(context.Background(), db, database.InsertRequest{Version: umapg.SchemaVersion + 1}); err != nil {
		t.Fatalf("mark newer: %v", err)
	}
}

// BreakMigrations makes new sessions on the database read-only by
// default: pg_dump, advisory locks and reads work, DDL fails. The role
// setting lives outside the database, so no backup captures it.
func (s *Server) BreakMigrations(t testing.TB, dsn string) (repair func()) {
	t.Helper()
	db := pgx.Identifier{Database(dsn)}.Sanitize()
	role := pgx.Identifier{User}.Sanitize()
	if _, err := s.admin.Exec(context.Background(), "ALTER ROLE "+role+" IN DATABASE "+db+" SET default_transaction_read_only = on"); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := s.admin.Exec(context.Background(), "ALTER ROLE "+role+" IN DATABASE "+db+" RESET default_transaction_read_only"); err != nil {
			t.Errorf("repair: %v", err)
		}
	}
}
