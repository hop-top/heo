package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"hop.top/heo/internal/storage"
	"hop.top/uma/model"
)

// Command is one pg_dump or pg_restore run.
type Command struct {
	// Path is the executable.
	Path string
	// Args never carry a secret: the password travels in Env.
	Args []string
	// Env is added to the inherited environment.
	Env []string
	// Stdin and Stdout, when set, are the process's.
	Stdin  io.Reader
	Stdout io.Writer
}

// Runner runs backup commands.
type Runner interface {
	Run(ctx context.Context, c Command) error
}

// ExecRunner runs commands as local processes.
type ExecRunner struct{}

// maxStderr bounds the stderr kept for an error message.
const maxStderr = 4 << 10

// Run starts c and waits for it. A failure carries the tail of stderr.
func (ExecRunner) Run(ctx context.Context, c Command) error {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...) //nolint:gosec // G204: the executable is the operator's configured pg_dump/pg_restore; arguments are built here
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdin, cmd.Stdout = c.Stdin, c.Stdout
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", filepath.Base(c.Path), err, msg)
		}
		return fmt.Errorf("%s: %w", filepath.Base(c.Path), err)
	}
	return nil
}

// tailBuffer keeps the last maxStderr bytes written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > maxStderr {
		t.b = t.b[len(t.b)-maxStderr:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

// reasonRe bounds the reason a backup file name records.
var reasonRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Backup dumps uma's tables in the store's schema with pg_dump, custom
// format, to a new owner-only file in the backup directory.
func (o *owner) Backup(ctx context.Context, reason string) (storage.Backup, error) {
	if !reasonRe.MatchString(reason) {
		return storage.Backup{}, model.NewError(model.Validation, "backup reason: want 1-32 lower-case letters, digits and '-'")
	}
	if err := o.Verify(ctx); err != nil {
		return storage.Backup{}, err
	}
	c := o.c
	if c.backupDir == "" {
		return storage.Backup{}, model.NewError(model.Validation, "storage.backup.dir: required for backups")
	}
	v, err := c.Versions(ctx)
	if err != nil {
		return storage.Backup{}, err
	}
	schemaName, err := c.currentSchema(ctx)
	if err != nil {
		return storage.Backup{}, err
	}
	if err := os.MkdirAll(c.backupDir, 0o700); err != nil {
		return storage.Backup{}, model.NewError(model.Unavailable, "backup directory: "+err.Error())
	}
	now := time.Now().UTC()
	b := storage.Backup{Driver: Name, Schema: v.Schema, Reason: reason, CreatedAt: now}
	b.Path = filepath.Join(c.backupDir, fmt.Sprintf("%s-%s-schema%d-%s.dump",
		Name, now.Format("20060102T150405.000000000Z"), v.Schema, reason))
	f, err := os.OpenFile(b.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return storage.Backup{}, model.NewError(model.Unavailable, "backup file: "+err.Error())
	}
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(b.Path)
		}
	}()
	err = c.runner.Run(ctx, Command{
		Path: c.pgDump,
		Args: []string{
			"--format=custom", "--no-password",
			"--table=" + tablePattern(schemaName),
			"--dbname=" + c.libpqDSN(),
		},
		Env:    c.passwordEnv(),
		Stdout: f,
	})
	if err != nil {
		return storage.Backup{}, toolError(ctx, "pg_dump", err)
	}
	if err := f.Sync(); err != nil {
		return storage.Backup{}, model.NewError(model.Unavailable, "backup file: "+err.Error())
	}
	if err := f.Close(); err != nil {
		return storage.Backup{}, model.NewError(model.Unavailable, "backup file: "+err.Error())
	}
	keep = true
	return b, nil
}

// Restore drops uma's tables, views and sequences in the store's
// schema, then restores the backup at path with pg_restore in one
// transaction. Dropping first removes relations a later migration added,
// which the backup does not know. If pg_restore fails the store is left without uma tables; the
// backup is untouched and the restore can be retried.
func (o *owner) Restore(ctx context.Context, path string) error {
	if err := o.Verify(ctx); err != nil {
		return err
	}
	c := o.c
	f, err := os.Open(path) //nolint:gosec // G304: the operator names the backup to restore
	if err != nil {
		return model.NewError(model.NotFound, "backup "+path+": "+errText(err))
	}
	defer func() { _ = f.Close() }()
	if err := checkDumpHeader(f); err != nil {
		return err
	}
	if err := c.dropUmaRelations(ctx); err != nil {
		return err
	}
	// Cached statements name the dropped tables.
	defer c.pool.Reset()
	err = c.runner.Run(ctx, Command{
		Path: c.pgRestore,
		Args: []string{
			"--no-password", "--no-owner", "--single-transaction", "--exit-on-error",
			"--dbname=" + c.libpqDSN(),
		},
		Env:   c.passwordEnv(),
		Stdin: f,
	})
	if err != nil {
		return toolError(ctx, "pg_restore", err)
	}
	return nil
}

// checkDumpHeader refuses a file that is not a pg_dump custom-format
// archive before anything is dropped, and rewinds f.
func checkDumpHeader(f *os.File) error {
	head := make([]byte, 5)
	if _, err := io.ReadFull(f, head); err != nil || !bytes.Equal(head, []byte("PGDMP")) {
		return model.NewError(model.Validation, "backup "+f.Name()+" is not a pg_dump custom-format archive")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return model.NewError(model.Unavailable, "backup "+f.Name()+": "+errText(err))
	}
	return nil
}

func (c *conn) currentSchema(ctx context.Context) (string, error) {
	var s string
	if err := c.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&s); err != nil {
		return "", queryError(ctx, err)
	}
	if s == "" {
		return "", model.NewError(model.Validation, "the connection has no current schema; check search_path")
	}
	return s, nil
}

// dropUmaRelations drops every uma_ table, view and sequence in the
// store's schema, the migration table included, in one transaction. A
// standalone sequence survives its tables' CASCADE, and pg_dump's table
// pattern dumps it, so the restore would collide with it.
func (c *conn) dropUmaRelations(ctx context.Context) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return queryError(ctx, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `SELECT format('DROP %s IF EXISTS %I.%I CASCADE',
	CASE c.relkind WHEN 'S' THEN 'SEQUENCE' WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' ELSE 'TABLE' END,
	n.nspname, c.relname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = current_schema() AND c.relname LIKE 'uma\_%' AND c.relkind IN ('r', 'p', 'v', 'm', 'S')
ORDER BY c.relkind = 'S', c.relname`)
	if err != nil {
		return queryError(ctx, err)
	}
	var stmts []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return queryError(ctx, err)
		}
		stmts = append(stmts, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return queryError(ctx, err)
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return queryError(ctx, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return queryError(ctx, err)
	}
	return nil
}

// tablePattern is pg_dump's pattern for uma's tables in schema: the
// schema quoted so it matches literally.
func tablePattern(schema string) string {
	return `"` + strings.ReplaceAll(schema, `"`, `""`) + `".uma_*`
}

// libpqDSN is the DSN for libpq tools. An empty one would make
// pg_restore write a script instead of connecting; the bare URL means
// libpq defaults, as the empty DSN does for pgx.
func (c *conn) libpqDSN() string {
	if c.dsn == "" {
		return "postgresql://"
	}
	return c.dsn
}

// passwordEnv hands the resolved password to a tool through its
// environment, never its arguments.
func (c *conn) passwordEnv() []string {
	if len(c.password) == 0 {
		return nil
	}
	return []string{"PGPASSWORD=" + string(c.password)}
}

func toolError(ctx context.Context, tool string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ee *exec.Error
	var pe *fs.PathError
	switch {
	case errors.As(err, &ee):
		return model.NewError(model.Unavailable, tool+" not runnable ("+errText(ee.Err)+"); install the PostgreSQL client tools or set storage.postgres."+tool)
	case errors.As(err, &pe):
		return model.NewError(model.Unavailable, tool+" not runnable ("+errText(pe.Err)+"); install the PostgreSQL client tools or set storage.postgres."+tool)
	}
	return model.NewError(model.Permanent, errText(err))
}

func errText(err error) string { return err.Error() }
