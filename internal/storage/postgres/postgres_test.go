package postgres_test

import (
	"log"
	"os"
	"strings"
	"testing"

	"hop.top/heo/internal/storage"
	"hop.top/heo/internal/storage/postgres"
	"hop.top/heo/internal/storage/postgres/pgtest"
	"hop.top/heo/internal/storage/storagetest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if err := pgtest.Terminate(); err != nil {
		log.Printf("terminate postgres container: %v", err)
	}
	os.Exit(code)
}

// harness provisions one database per store in the shared server, and
// runs pg_dump and pg_restore inside its container.
type harness struct {
	driver *postgres.Driver
	runner *pgtest.Runner
}

func newHarness(t *testing.T) *harness {
	r := pgtest.Require(t).Runner()
	return &harness{driver: postgres.New(postgres.Options{Runner: r}), runner: r}
}

func (h *harness) Driver() storage.Driver { return h.driver }

func (h *harness) NewStore(t *testing.T) storage.Config { return pgtest.Require(t).NewStore(t) }

func dsn(cfg storage.Config) string { return cfg.Settings.GetString(postgres.KeyDSN) }

func (h *harness) Downgrade(t *testing.T, cfg storage.Config) {
	pgtest.Require(t).Downgrade(t, dsn(cfg))
}

func (h *harness) MarkNewer(t *testing.T, cfg storage.Config) {
	pgtest.Require(t).MarkNewer(t, dsn(cfg))
}

func (h *harness) BreakMigrations(t *testing.T, cfg storage.Config) func() {
	return pgtest.Require(t).BreakMigrations(t, dsn(cfg))
}

func TestConformance(t *testing.T) {
	h := newHarness(t)
	storagetest.Run(t, h)
	// Backups and restores ran, and the password never reached a
	// command line.
	runs := h.runner.Commands()
	if len(runs) == 0 {
		t.Fatal("no backup command ran")
	}
	for _, c := range runs {
		if strings.Contains(strings.Join(c.Args, " "), pgtest.Password) {
			t.Errorf("password in %s arguments: %q", c.Path, c.Args)
		}
		found := false
		for _, e := range c.Env {
			if e == "PGPASSWORD="+pgtest.Password {
				found = true
			}
		}
		if !found {
			t.Errorf("%s ran without the resolved password in its environment", c.Path)
		}
	}
}
