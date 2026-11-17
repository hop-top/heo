package e2e

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	umapg "hop.top/uma/store/postgres"
)

// pgImage is the server the runtime runs against in these tests, the
// one heo's storage driver tests pin.
const pgImage = "postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15"

// requireDockerEnv makes a missing container runtime fail the tests
// instead of skipping them.
const requireDockerEnv = "HEO_REQUIRE_DOCKER"

var pg struct {
	once  sync.Once
	err   error
	ctr   *tcpostgres.PostgresContainer
	admin *pgxpool.Pool
	host  string
	next  atomic.Int64
}

// stopPostgres terminates the test server, if one was started.
func stopPostgres() {
	if pg.admin != nil {
		pg.admin.Close()
	}
	if pg.ctr != nil {
		if err := testcontainers.TerminateContainer(pg.ctr); err != nil {
			log.Printf("terminate postgres container: %v", err)
		}
	}
}

func requirePostgres(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the daemon is unix-only")
	}
	if os.Getenv(requireDockerEnv) != "1" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
	}
	pg.once.Do(func() { pg.err = startPostgres() })
	if pg.err != nil {
		t.Fatalf("postgres test server: %v", pg.err)
	}
}

func startPostgres() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// Trust authentication: the daemon under test connects without a
	// password, so no credential backend is involved.
	ctr, err := tcpostgres.Run(ctx, pgImage,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("heo"),
		tcpostgres.WithPassword("unused"),
		testcontainers.WithEnv(map[string]string{"POSTGRES_HOST_AUTH_METHOD": "trust"}),
		tcpostgres.BasicWaitStrategies(),
	)
	pg.ctr = ctr
	if err != nil {
		return fmt.Errorf("start postgres container: %w", err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return err
	}
	if pg.admin, err = pgxpool.New(ctx, dsn); err != nil {
		return err
	}
	cfg := pg.admin.Config().ConnConfig
	pg.host = fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	return nil
}

// testDB is one throwaway database.
type testDB struct {
	t    *testing.T
	name string
	dsn  string
	// blobs, when set, is the attachment store the test's own engines
	// (as, operator) use: the daemon's, so what they redact is gone
	// from the bytes the daemon serves.
	blobs ports.BlobStore
}

// newDB creates an empty database, dropped when the test ends.
func newDB(t *testing.T) *testDB {
	t.Helper()
	requirePostgres(t)
	name := fmt.Sprintf("heo_e2e_%d", pg.next.Add(1))
	if _, err := pg.admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		drop := "DROP DATABASE " + pgx.Identifier{name}.Sanitize() + " WITH (FORCE)"
		if _, err := pg.admin.Exec(context.Background(), drop); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})
	return &testDB{t: t, name: name, dsn: fmt.Sprintf("postgres://heo@%s/%s?sslmode=disable", pg.host, name)}
}

func (db *testDB) pool() *pgxpool.Pool {
	db.t.Helper()
	p, err := pgxpool.New(db.t.Context(), db.dsn)
	if err != nil {
		db.t.Fatal(err)
	}
	db.t.Cleanup(p.Close)
	return p
}

// migrate brings the database to this binary's schema, as heo's storage
// migration would.
func (db *testDB) migrate() *testDB {
	db.t.Helper()
	if err := umapg.Migrate(db.t.Context(), db.pool()); err != nil {
		db.t.Fatalf("migrate: %v", err)
	}
	return db
}

// downgrade rolls the newest migration back, as the previous release
// left the schema: a pending migration.
func (db *testDB) downgrade() {
	db.t.Helper()
	db.downgradeTo(umapg.SchemaVersion - 1)
}

// downgradeTo rolls migrations back down to version, as the release
// that last migrated to it left the schema.
func (db *testDB) downgradeTo(version int64) {
	db.t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "hop.top/uma/store/postgres").Output()
	if err != nil {
		db.t.Fatalf("go list uma store module: %v", err)
	}
	dir := filepath.Join(strings.TrimSpace(string(out)), "migrations")
	sqlDB := stdlib.OpenDBFromPool(db.pool())
	defer func() { _ = sqlDB.Close() }()
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS(dir),
		goose.WithDisableGlobalRegistry(true), goose.WithTableName(umapg.VersionTable))
	if err != nil {
		db.t.Fatal(err)
	}
	if _, err := p.DownTo(db.t.Context(), version); err != nil {
		db.t.Fatalf("downgrade: %v", err)
	}
}

// channel configures a fake channel in tenant's store, as a channel
// command would, and returns its ID.
func (db *testDB) channel(tenant model.TenantID, key, account string) model.ID {
	db.t.Helper()
	ctx := db.t.Context()
	st, err := umapg.Open(ctx, db.pool(), umapg.Options{})
	if err != nil {
		db.t.Fatal(err)
	}
	e, err := uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{adapterstest.New()},
		Clock:    clock{},
		IDs:      ids{},
	})
	if err != nil {
		db.t.Fatal(err)
	}
	defer func() { _ = e.Close(context.Background()) }()
	c, err := e.For(model.Principal{Tenant: tenant, Actor: "e2e"})
	if err != nil {
		db.t.Fatal(err)
	}
	acc, err := c.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{
		Command: model.Command{Call: model.Call{Correlation: "e2e"}, Operation: model.OperationID("channel-" + key)},
		Channel: model.Channel{
			Ref: model.ChannelRef{Tenant: tenant}, Key: key, Type: adapterstest.Type,
			Configuration: adapterstest.Config{Account: account}.Configuration(),
		},
	})
	if err != nil {
		db.t.Fatalf("configure channel %s: %v", key, err)
	}
	ref, err := acc.Operation.Result.AsChannel()
	if err != nil {
		db.t.Fatalf("configure channel %s: %v", key, err)
	}
	return ref.ID
}

type clock struct{}

func (clock) Now() time.Time { return time.Now() }

type ids struct{}

func (ids) New(context.Context) (model.ID, error) {
	id, err := uuid.NewV7()
	return model.ID(id.String()), err
}

// useStore points every profile of the sandbox at db, with the file
// credential backend so no keychain is touched.
func (s *sandbox) useStore(db *testDB) {
	s.write("cfg/heo/config.yaml", fmt.Sprintf(
		"credentials:\n  backend: file\nstorage:\n  postgres:\n    dsn: %q\n", db.dsn))
}

// profileStore points one named profile at db.
func (s *sandbox) profileStore(name string, db *testDB) {
	s.write("cfg/heo/profiles/"+name+".yaml", fmt.Sprintf(
		"credentials:\n  backend: file\nstorage:\n  postgres:\n    dsn: %q\n", db.dsn))
}

var e2eBin = sync.OnceValues(func() (string, error) {
	out := filepath.Join(filepath.Dir(heoBin), "heo-e2e")
	build := exec.Command("go", "build", "-buildvcs=false", "-tags", "heoe2e", "-o", out, "hop.top/heo") //nolint:gosec // output path is the test's own temp dir
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	return out, build.Run()
})

// withE2EBinary runs fn with the e2e build, which carries the fake
// adapter, as the binary under test.
func withE2EBinary(t *testing.T, fn func()) {
	t.Helper()
	bin, err := e2eBin()
	if err != nil {
		t.Fatalf("build heo with the e2e adapter: %v", err)
	}
	prev := heoBin
	heoBin = bin
	defer func() { heoBin = prev }()
	fn()
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec,noctx // test URL on loopback
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestRuntimeEmptyInstallReadyAndDrains: with no channel configured,
// a bare start brings the socket, workers and receivers up, plus the
// webhook listener when enabled; /readyz answers ready; the pidfile
// names the daemon; SIGTERM drains and exits 0 and removes the pidfile.
func TestRuntimeEmptyInstallReadyAndDrains(t *testing.T) {
	db := newDB(t).migrate()
	s := newSandbox(t)
	s.useStore(db)
	hook := freeAddr(t)
	d := s.start(nil, nil, []string{"socket", "workers", "receivers", "webhook"},
		"-c", "services.webhook.addr="+hook, "service", "start", "--enable", "webhook")
	if got := d.addr("webhook"); got != hook {
		t.Errorf("webhook at %q, want %q", got, hook)
	}
	code, body := httpGet(t, "http://"+hook+"/readyz")
	if code != http.StatusOK || !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("/readyz = %d %s", code, body)
	}
	if code, _ := httpGet(t, "http://"+hook+"/webhooks/nosuch"); code != http.StatusNotFound {
		t.Errorf("unknown webhook route = %d, want 404", code)
	}
	pidPath := s.path("run", "heo", "default", "heo.pid")
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(pidPath) //nolint:gosec // path under the test sandbox
		if err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(d.cmd.Process.Pid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pidfile %s = %q, %v; want pid %d", pidPath, b, err, d.cmd.Process.Pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code := d.term(); code != 0 {
		t.Fatalf("SIGTERM exit %d:\n%s", code, d.log())
	}
	log := d.log()
	for _, want := range []string{"runtime: workers drained", "reason=clean-stop"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	// Ingress stops before workers drain, the event bridge reads what
	// they committed, and the control socket outlives them all: kit
	// stops in reverse start order.
	order := stopOrder(log)
	if want := []string{"webhook", "receivers", "workers", "bridge", "socket"}; strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("stop order %v, want %v", order, want)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("pidfile left behind: %v", err)
	}
}

// TestRuntimeApprovalChatRegisteredOnlyWhenEnabled: held_operations.chat
// enabled brings the approval-chat service up, and it stops after the
// workers drain and before the bridge, which stops last of the three.
func TestRuntimeApprovalChatRegisteredOnlyWhenEnabled(t *testing.T) {
	db := newDB(t).migrate()
	s := newSandbox(t)
	s.useStore(db)
	s.write("cfg/heo/profiles/default.yaml", "held_operations:\n  chat:\n    enabled: true\n    conversations:\n"+
		"      - inbox: inbox-1\n        conversation: conv-1\n        members:\n"+
		"          - identity: ident-1\n            actor: uid:1001\n")
	d := s.start(nil, nil, []string{"socket", "workers", "receivers", "approval-chat"}, "service", "start")
	if code := d.term(); code != 0 {
		t.Fatalf("SIGTERM exit %d:\n%s", code, d.log())
	}
	order := stopOrder(d.log())
	if want := []string{"receivers", "workers", "approval-chat", "bridge", "socket"}; strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("stop order %v, want %v", order, want)
	}
}

// stopOrder lists the services in the order the log reports them
// stopped.
func stopOrder(log string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, "serve: stopped object=service") {
			continue
		}
		if i := strings.Index(line, "service="); i >= 0 {
			out = append(out, strings.Fields(line[i+len("service="):])[0])
		}
	}
	return out
}

// TestRuntimeSecondDaemonOnSameStoreRefused: a second daemon, another
// profile on the same store, is refused at start and names the owner;
// the first keeps running and still stops cleanly.
func TestRuntimeSecondDaemonOnSameStoreRefused(t *testing.T) {
	db := newDB(t).migrate()
	s := newSandbox(t)
	s.useStore(db)
	s.write("cfg/heo/profiles/other.yaml", "")
	first := s.start(nil, nil, []string{"socket", "workers", "receivers"}, "service", "start")
	r := s.run(nil, "--profile", "other", "service", "start", "workers")
	s.want(r, 1, "--profile", "other", "service", "start", "workers")
	if !strings.Contains(r.stderr, "owned by another heo") {
		t.Errorf("refusal does not name the owner:\n%s", r.stderr)
	}
	if code := first.term(); code != 0 {
		t.Fatalf("first daemon exit %d:\n%s", code, first.log())
	}
}

// TestRuntimePendingMigrationRefused: a store one migration behind is
// refused before any worker starts, with the way forward.
func TestRuntimePendingMigrationRefused(t *testing.T) {
	db := newDB(t).migrate()
	db.downgrade()
	s := newSandbox(t)
	s.useStore(db)
	r := s.run(nil, "service", "start")
	s.want(r, 1, "service", "start")
	for _, want := range []string{
		fmt.Sprintf("store schema version %d is older than %d", umapg.SchemaVersion-1, umapg.SchemaVersion),
		"run `heo storage migrate`",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}
	if m := readyRe.FindAllStringSubmatch(r.stderr, -1); len(m) > 0 {
		for _, sm := range m {
			if sm[1] == "workers" || sm[1] == "receivers" {
				t.Errorf("%s reported ready on an old schema:\n%s", sm[1], r.stderr)
			}
		}
	}
}

// TestRuntimeDuplicateProviderAccountRefused: two channels of one
// profile on one provider account refuse the start; nothing receives.
func TestRuntimeDuplicateProviderAccountRefused(t *testing.T) {
	db := newDB(t).migrate()
	db.channel("default", "alpha", "acct-1")
	db.channel("default", "beta", "acct-1")
	s := newSandbox(t)
	s.useStore(db)
	withE2EBinary(t, func() {
		r := s.run(nil, "service", "start")
		s.want(r, 1, "service", "start")
		for _, want := range []string{"receive the same provider account", `"alpha"`, `"beta"`} {
			if !strings.Contains(r.stderr, want) {
				t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
			}
		}
	})
}

// TestRuntimeProviderAccountClaimedAcrossProfiles: a provider account
// one profile's daemon receives refuses another profile's daemon on the
// same host, though their stores differ; distinct accounts receive
// side by side, and the webhook routes a configured channel.
func TestRuntimeProviderAccountClaimedAcrossProfiles(t *testing.T) {
	dbA, dbB := newDB(t).migrate(), newDB(t).migrate()
	idA := dbA.channel("default", "alpha", "acct-shared")
	dbB.channel("other", "beta", "acct-shared")
	s := newSandbox(t)
	s.useStore(dbA)
	s.profileStore("other", dbB)
	withE2EBinary(t, func() {
		hook := freeAddr(t)
		first := s.start(nil, nil, []string{"workers", "receivers", "webhook"},
			"-c", "services.webhook.addr="+hook, "service", "start", "--enable", "webhook")
		resp, err := http.Post("http://"+hook+"/webhooks/"+string(idA), "application/json", strings.NewReader("{}")) //nolint:gosec,noctx // loopback test URL
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("channel webhook = %d, want the adapter's 204", resp.StatusCode)
		}
		r := s.run(nil, "--profile", "other", "service", "start", "receivers")
		s.want(r, 1, "--profile", "other", "service", "start", "receivers")
		if !strings.Contains(r.stderr, "already received by heo/default (pid ") {
			t.Errorf("refusal does not name the holder:\n%s", r.stderr)
		}
		if code := first.term(); code != 0 {
			t.Fatalf("first daemon exit %d:\n%s", code, first.log())
		}
		// Released at exit: the other profile now receives the account.
		second := s.start(nil, nil, []string{"receivers"}, "--profile", "other", "service", "start", "receivers")
		if code := second.term(); code != 0 {
			t.Fatalf("second daemon exit %d:\n%s", code, second.log())
		}
	})
}

// TestConfigValidateRuntime: config validate checks the runtime
// services' settings offline.
func TestConfigValidateRuntime(t *testing.T) {
	s := newSandbox(t)
	args := []string{"config", "validate", "--format", "json",
		"-c", "services.webhook.addr=0.0.0.0:8083", "-c", "services.workers.count=0"}
	r := s.run(nil, args...)
	s.want(r, 2, args...)
	for _, key := range []string{`"key": "services.webhook.addr"`, `"key": "services.workers.count"`} {
		if !strings.Contains(r.stdout, key) {
			t.Errorf("no issue for %s:\n%s", key, r.stdout)
		}
	}
	args = []string{"config", "validate", "-c", "services.webhook.enabled=true"}
	r = s.run(nil, args...)
	s.want(r, 0, args...)
	if !strings.Contains(r.stdout+r.stderr, "services.webhook.public_base_url") {
		t.Errorf("no warning for an enabled webhook without a public URL:\n%s%s", r.stdout, r.stderr)
	}
}
