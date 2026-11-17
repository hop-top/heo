package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/errmap"
	"hop.top/heo/internal/profile"
	"hop.top/heo/internal/settings"
	"hop.top/heo/internal/storage"
	"hop.top/heo/internal/storage/bundle"
	"hop.top/heo/internal/version"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// Backup reasons the storage commands record.
const (
	reasonManual     = "manual"
	reasonPreRestore = "pre-restore"
)

// storageCmd builds the non-runnable `storage` group: the selected
// profile's data store, inspected, backed up, restored and migrated.
func (a *app) storageCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "storage",
		Short: "Inspect, back up, restore and migrate the profile's data store",
		Long: `Inspect, back up, restore and migrate the data store the selected
profile's daemon runs on (storage.driver, postgres by default).

status reads versions and needs nothing else. backup, restore and
migrate take exclusive ownership of the store, the lease a running
daemon holds: while the daemon runs they are refused (exit 4); stop it
with heo service stop first.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.storageStatusCmd(), a.storageBackupCmd(), a.storageRestoreCmd(), a.storageMigrateCmd())
	return grp
}

// heoCommand spells a heo command line for the selected profile, for
// suggested fixes.
func (a *app) heoCommand(sub string) string {
	if a.state == nil || a.state.Profile() == profile.Default() {
		return "heo " + sub
	}
	return "heo --profile " + a.state.Profile().String() + " " + sub
}

// connectStorage connects to the selected profile's store without
// taking ownership. The caller closes the connection.
func (a *app) connectStorage(ctx context.Context) (storage.Conn, storage.Config, error) {
	d, cfg, err := storageConfig(ctx, a.storage, a.r.Viper, a.state.Profile())
	if err != nil {
		e := errmap.Of(err)
		e.SuggestedFix = "check the storage settings with `" + a.heoCommand("config validate") + "`"
		return nil, cfg, e
	}
	c, err := d.Connect(ctx, cfg)
	if err != nil {
		e := errmap.Of(err)
		if e.SuggestedFix == "" {
			e.SuggestedFix = "check storage." + d.Name() + ".* with `" + a.heoCommand("config validate") +
				"` and that the database is reachable"
		}
		return nil, cfg, e
	}
	return c, cfg, nil
}

// ownStorage connects and takes exclusive ownership of the store for
// verb. A store owned elsewhere, by a running daemon, is refused with
// CONFLICT; ownership is never forced. The owner backs up and restores
// archives that carry the profile's blob directory with the store.
// release gives ownership and the connection back.
func (a *app) ownStorage(ctx context.Context, verb string) (*bundle.Owner, storage.Config, func(), error) {
	c, cfg, err := a.connectStorage(ctx)
	if err != nil {
		return nil, cfg, nil, err
	}
	o, err := c.Acquire(ctx)
	if err != nil {
		_ = c.Close()
		e := errmap.Of(err)
		if model.CodeOf(err) == model.Conflict {
			e.Message = "heo storage " + verb + " needs exclusive ownership of the data store: " + e.Message
			e.SuggestedFix = "stop the daemon with `" + a.heoCommand("service stop") + "`, then rerun"
		}
		return nil, cfg, nil, e
	}
	release := func() {
		_ = o.Release(context.WithoutCancel(ctx))
		_ = c.Close()
	}
	return bundle.Wrap(o, cfg.BackupDir, a.blobTree()), cfg, release, nil
}

// blobTree is the profile's blob directory, the part of its attachments
// heo carries in backups. Nil when none is configured: a blob backend
// that is not a local directory has no tree and backs up its own bytes.
func (a *app) blobTree() bundle.Tree {
	dir := a.r.Viper.GetString(settings.KeyStorageBlobsDir)
	if dir == "" {
		return nil
	}
	return bundle.LocalDir(dir)
}

// blobStatus is what `storage status` reports of the blob directory.
type blobStatus struct {
	Backend string `json:"backend"         yaml:"backend"`
	Dir     string `json:"dir"             yaml:"dir"`
	Exists  bool   `json:"exists"          yaml:"exists"`
	Files   int    `json:"files"           yaml:"files"`
	Bytes   int64  `json:"bytes"           yaml:"bytes"`
	Error   string `json:"error,omitempty" yaml:"error,omitempty"`
}

// blobsStatus counts the blob directory's files without reading them. A
// directory that cannot be counted is reported, not fatal: status is
// about the store.
func (a *app) blobsStatus(ctx context.Context) *blobStatus {
	t := a.blobTree()
	if t == nil {
		return nil
	}
	st, err := bundle.Inspect(ctx, t)
	out := &blobStatus{Backend: "local", Dir: st.Dir, Exists: st.Exists, Files: st.Files, Bytes: st.Bytes}
	if err != nil {
		out.Error = errmap.Of(err).Message
	}
	return out
}

// structured reports whether the active format is json or yaml: the
// commands' full result; any other format gets their text.
func (a *app) structured() bool {
	switch a.r.Viper.GetString("format") {
	case output.JSON, output.YAML:
		return true
	}
	return false
}

// storageAction is what to do about a store's compatibility, empty when
// nothing.
func (a *app) storageAction(c storage.Compat) string {
	switch c {
	case storage.CompatUninitialized:
		return "initialize it with `" + a.heoCommand("storage migrate") + "`"
	case storage.CompatMigrate:
		return "back it up and migrate it with `" + a.heoCommand("storage migrate") + "`"
	case storage.CompatNewer:
		return "a newer heo migrated it: upgrade heo with `heo upgrade`, or restore a backup taken before that migration with `" +
			a.heoCommand("storage restore <path>") + "`"
	}
	return ""
}

// storageStatus is what `storage status` reports.
type storageStatus struct {
	Profile string         `json:"profile"          yaml:"profile"`
	Heo     string         `json:"heo"              yaml:"heo"`
	Kit     string         `json:"kit"              yaml:"kit"`
	Uma     string         `json:"uma"              yaml:"uma"`
	Storage storage.Report `json:"storage"          yaml:"storage"`
	Blobs   *blobStatus    `json:"blobs,omitempty"  yaml:"blobs,omitempty"`
	Action  string         `json:"action,omitempty" yaml:"action,omitempty"`
}

func (a *app) storageStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Show heo, kit, uma and store versions and schema compatibility",
		Long: `Show the versions of heo, kit and uma this binary was built with, the
store's driver, server and store library, the schema version the store
holds against the one this binary implements, and their compatibility:

  compatible          the daemon can start on it
  uninitialized       no schema yet; storage migrate creates it
  migration_required  older; storage migrate backs it up, then migrates
  newer_schema        a newer heo migrated it; this binary refuses it

It also counts the attachment files in the blob directory
(storage.blobs.dir) and their size, without reading them. There is no
cheap way to tell which attachment records lack their file, so that is
not checked; the count is what backup would carry.

Read-only: no ownership is taken, so it works while the daemon runs.
Exits 0 whatever the compatibility; an unreachable store exits 6.`,
		Example: `  heo storage status
  heo storage status --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			conn, _, err := a.connectStorage(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			rep, err := storage.Inspect(ctx, conn)
			if err != nil {
				return errmap.Of(err)
			}
			s := storageStatus{
				Profile: a.state.Profile().String(),
				Heo:     version.Version(),
				Kit:     version.Module("hop.top/kit"),
				Uma:     version.Module("hop.top/uma"),
				Storage: rep,
				Blobs:   a.blobsStatus(ctx),
				Action:  a.storageAction(rep.Compat),
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, s)
			}
			return writeStatus(cmd.OutOrStdout(), s)
		},
	}
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	return c
}

func (b *blobStatus) text() string {
	switch {
	case b.Error != "":
		return b.Dir + " (not counted: " + b.Error + ")"
	case !b.Exists:
		return b.Dir + " (not created yet)"
	}
	return fmt.Sprintf("%s (%d files, %s)", b.Dir, b.Files, humanBytes(b.Bytes))
}

// humanBytes spells n in binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func writeStatus(w io.Writer, s storageStatus) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	rows := [][2]string{
		{"profile", s.Profile},
		{"heo", s.Heo},
		{"kit", s.Kit},
		{"uma", s.Uma},
		{"driver", s.Storage.Driver},
		{"server", s.Storage.Server},
		{"library", s.Storage.Library},
		{"schema", fmt.Sprintf("%d (this binary: %d)", s.Storage.Schema, s.Storage.LibrarySchema)},
		{"compat", string(s.Storage.Compat)},
	}
	if b := s.Blobs; b != nil {
		rows = append(rows, [2]string{"blobs", b.text()})
	}
	if s.Action != "" {
		rows = append(rows, [2]string{"action", s.Action})
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", r[0], r[1]); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func (a *app) storageBackupCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "backup",
		Short: "Back up the profile's data store and attachments to a new owner-only file",
		Long: `Write a backup of the store's data and schema, and of the attachment
files in the blob directory (storage.blobs.dir), to one new owner-only
(0600) file under storage.backup.dir, and print its path: alone on
stdout by default, with driver, schema, reason, time and the attachment
count and size under --format json or yaml. The file is a tar archive:
a manifest listing every entry with its size and SHA-256 checksum, the
driver's dump (for postgres, a pg_dump custom-format archive of uma's
tables, written with storage.postgres.pg_dump), and the blob tree.

Takes exclusive ownership of the store: refused (exit 4) while the
daemon runs. A store with no schema yet has nothing to back up (exit
4). --dry-run checks ownership and prints the plan.`,
		Example: `  heo storage backup
  heo storage backup --format json
  heo storage backup --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			o, cfg, release, err := a.ownStorage(ctx, "backup")
			if err != nil {
				return err
			}
			defer release()
			v, err := o.Versions(ctx)
			if err != nil {
				return errmap.Of(err)
			}
			if v.Schema <= 0 {
				e := output.ConflictError("the data store holds no schema yet; there is nothing to back up")
				e.SuggestedFix = "initialize it with `" + a.heoCommand("storage migrate") + "`"
				return e
			}
			if cli.IsDryRun(cmd) {
				return cli.RenderPlan(cmd, cli.Plan{
					Effects: []cli.Effect{{Kind: "create", Target: cfg.BackupDir, Reversible: true,
						Detail: fmt.Sprintf("%s backup of schema %d%s, owner-only file", v.Driver, v.Schema, a.blobsDetail(ctx))}},
					PrerequisitesChecked: []string{"ownership"},
				})
			}
			b, err := o.Backup(ctx, reasonManual)
			if err != nil {
				return errmap.Of(err)
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, b)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), b.Path)
			return err
		},
	}
	cli.SetSideEffect(c, cli.SideEffectWriteLocal)
	cli.SetIdempotency(c, cli.IdempotencyNo)
	return c
}

// preRestoreBlobs spells what the pre-restore backup adds to a plan.
func (a *app) preRestoreBlobs(ctx context.Context, arch *bundle.Archive) string {
	if !arch.HasBlobs() {
		return ""
	}
	return a.blobsDetail(ctx)
}

// blobsDetail spells the blob directory's size for a plan, empty when
// the profile has none.
func (a *app) blobsDetail(ctx context.Context) string {
	b := a.blobsStatus(ctx)
	switch {
	case b == nil || b.Error != "":
		return ""
	case !b.Exists:
		return " and an empty blob directory"
	}
	return fmt.Sprintf(" and %d attachment files (%s)", b.Files, humanBytes(b.Bytes))
}

// restoreResult is what `storage restore` reports.
type restoreResult struct {
	Restored         string          `json:"restored"                     yaml:"restored"`
	PreRestoreBackup *storage.Backup `json:"pre_restore_backup,omitempty" yaml:"pre_restore_backup,omitempty"`
	// Blobs counts the attachment files now in the blob directory; nil
	// when the backup carried none and the directory was left alone.
	Blobs          *storage.BlobStats `json:"blobs,omitempty"           yaml:"blobs,omitempty"`
	BlobsUntouched bool               `json:"blobs_untouched,omitempty" yaml:"blobs_untouched,omitempty"`
	// ReplacedBlobs is where the previous blob directory was kept when no
	// backup holds it.
	ReplacedBlobs string         `json:"replaced_blobs,omitempty" yaml:"replaced_blobs,omitempty"`
	Schema        int64          `json:"schema"                       yaml:"schema"`
	Compat        storage.Compat `json:"compat"                       yaml:"compat"`
	Action        string         `json:"action,omitempty"             yaml:"action,omitempty"`
}

func (a *app) storageRestoreCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "restore <path>",
		Short: "Replace the profile's data store and attachments with a backup",
		Long: `Replace the store's data and schema, and the attachment files in the
blob directory, with the backup at <path>, as written by heo storage
backup or before a migration.

The backup is read through first: its manifest must be one this heo
reads, from the same storage driver and a schema this binary runs, and
every entry must match the manifest's size and checksum. A backup that
does not (exit 2), or holds a schema newer than this binary's (exit 1),
is refused before anything is changed. The blob directory is staged
beside the live one and swapped in whole, so it is never half restored;
if the store then fails to restore, the old directory is put back.

A backup with no blobs, one written before attachments were backed up
or the backup before a migration, restores the store only and leaves
the blob directory as it is; the result says so.

Destructive: it asks for confirmation on a terminal; without one it is
refused (exit 5) unless --confirm=yes is passed. It never prompts
without a terminal. Before replacing an initialized store it backs the
current data up, attachments included (reason pre-restore), and reports
that file: restoring it undoes the restore. When nothing backs up the
replaced blob directory, it is kept beside the directory and reported.
A failed restore leaves the backup untouched and can be retried.

The restored schema is the backup's: a backup taken before a migration
restores the older schema, and heo storage migrate brings it current.
Takes exclusive ownership of the store: refused (exit 4) while the
daemon runs. A missing file exits 3. --dry-run checks the file and
ownership and prints the plan without asking.`,
		Example: `  heo storage restore ~/.local/share/heo/default/backups/postgres-20261011T120000.000000000Z-schema12-manual.tar
  heo storage restore ./backup.tar --confirm=yes --format json
  heo storage restore ./backup.tar --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			path, err := filepath.Abs(args[0])
			if err != nil {
				return output.UsageError(err.Error())
			}
			switch fi, err := os.Stat(path); {
			case err != nil:
				e := output.NotFoundError("backup " + path + ": " + err.Error())
				e.SuggestedFix = "name a file written by `" + a.heoCommand("storage backup") + "`"
				return e
			case !fi.Mode().IsRegular():
				return output.UsageError("backup " + path + " is not a regular file")
			}
			o, cfg, release, err := a.ownStorage(ctx, "restore")
			if err != nil {
				return err
			}
			defer release()
			v, err := o.Versions(ctx)
			if err != nil {
				return errmap.Of(err)
			}
			arch, err := bundle.Examine(ctx, path)
			if err == nil {
				err = o.Accept(arch, v)
			}
			if err != nil {
				e := errmap.Of(err)
				if model.CodeOf(err) == model.SchemaMismatch {
					e.SuggestedFix = "upgrade heo with `heo upgrade`, then restore again"
				} else {
					e.SuggestedFix = "the backup is untouched and nothing was changed; use a file written by `" +
						a.heoCommand("storage backup") + "`"
				}
				return e
			}
			if cli.IsDryRun(cmd) {
				var effects []cli.Effect
				if v.Schema > 0 {
					effects = append(effects, cli.Effect{Kind: "create", Target: cfg.BackupDir, Reversible: true,
						Detail: fmt.Sprintf("pre-restore backup of the current data, schema %d%s", v.Schema,
							a.preRestoreBlobs(ctx, arch))})
				}
				effects = append(effects, cli.Effect{Kind: "replace", Target: "storage:" + v.Driver,
					Reversible: v.Schema > 0, Detail: "data and schema from " + path})
				if arch.HasBlobs() {
					effects = append(effects, cli.Effect{Kind: "replace", Target: "blobs:" + o.Tree().Dir(),
						Reversible: v.Schema > 0, Detail: fmt.Sprintf("%d attachment files (%s) from %s",
							arch.Manifest.Blobs.Count, humanBytes(arch.Manifest.Blobs.Bytes), path)})
				}
				return cli.RenderPlan(cmd, cli.Plan{Effects: effects, PrerequisitesChecked: []string{"backup-file", "checksums", "ownership"}})
			}
			res := restoreResult{Restored: path}
			if v.Schema > 0 {
				b, err := o.Snapshot(ctx, reasonPreRestore, arch.HasBlobs())
				if err != nil {
					e := errmap.Of(err)
					e.Message = "backing up the current data failed; nothing was restored: " + e.Message
					return e
				}
				res.PreRestoreBackup = &b
			}
			covered := res.PreRestoreBackup != nil && res.PreRestoreBackup.Blobs != nil
			rr, err := o.RestoreArchive(ctx, arch, bundle.RestoreOptions{Covered: covered})
			if err != nil {
				e := errmap.Of(err)
				e.Message = "restore of " + path + " failed: " + e.Message
				e.SuggestedFix = "the backup is untouched; fix the cause and rerun"
				if res.PreRestoreBackup != nil {
					e.SuggestedFix += ", or restore the data it replaced from " + res.PreRestoreBackup.Path
				}
				return e
			}
			res.Blobs, res.ReplacedBlobs, res.BlobsUntouched = rr.Blobs, rr.Replaced, !arch.HasBlobs()
			rep, err := storage.Inspect(ctx, o)
			if err != nil {
				return errmap.Of(err)
			}
			res.Schema, res.Compat, res.Action = rep.Schema, rep.Compat, a.storageAction(rep.Compat)
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			text := fmt.Sprintf("restored %s: schema %d, %s\n", res.Restored, res.Schema, res.Compat)
			switch {
			case res.Blobs != nil:
				text += fmt.Sprintf("blob directory: %d attachment files restored (%s)\n", res.Blobs.Files, humanBytes(res.Blobs.Bytes))
			default:
				text += "blob directory: untouched; the backup carries no attachments\n"
			}
			if res.PreRestoreBackup != nil {
				text += "replaced data backed up to " + res.PreRestoreBackup.Path + "\n"
			}
			if res.ReplacedBlobs != "" {
				text += "replaced blob directory kept at " + res.ReplacedBlobs + "; delete it once the restore is checked\n"
			}
			if res.Action != "" {
				text += "next: " + res.Action + "\n"
			}
			_, err = io.WriteString(cmd.OutOrStdout(), text)
			return err
		},
	}
	cli.SetSideEffect(c, cli.SideEffectDestructiveLocal)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	return c
}

func (a *app) storageMigrateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "migrate",
		Short: "Bring the profile's data store to this binary's schema",
		Long: `Apply the store library's pending schema migrations, so the daemon can
start on the store:

  - an older store is backed up first (reason pre-migrate); a failed
    backup migrates nothing;
  - an empty store is initialized; it has nothing to back up;
  - a current store is left alone;
  - a store a newer heo migrated is refused (exit 1, SCHEMA_MISMATCH)
    before anything is written: heo never downgrades a schema.

Reports the schema versions before and after and the backup taken. A
failed migration leaves the store at its old schema, names the backup,
and exits non-zero; restore that backup with heo storage restore if the
store is unusable. Takes exclusive ownership of the store: refused
(exit 4) while the daemon runs. --dry-run prints the plan; a newer
schema is refused the same way.`,
		Example: `  heo storage migrate
  heo storage migrate --dry-run
  heo storage migrate --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			o, cfg, release, err := a.ownStorage(ctx, "migrate")
			if err != nil {
				return err
			}
			defer release()
			if cli.IsDryRun(cmd) {
				p, err := storage.PlanMigrate(ctx, o)
				if err != nil {
					return a.migrateError(err, nil)
				}
				var effects []cli.Effect
				if p.Backup {
					effects = append(effects, cli.Effect{Kind: "create", Target: cfg.BackupDir, Reversible: true,
						Detail: fmt.Sprintf("pre-migrate backup of schema %d", p.From)})
				}
				if p.From != p.To {
					effects = append(effects, cli.Effect{Kind: "migrate", Target: "storage", Reversible: p.Backup,
						Detail: fmt.Sprintf("schema %d → %d", p.From, p.To)})
				}
				return cli.RenderPlan(cmd, cli.Plan{Effects: effects, PrerequisitesChecked: []string{"ownership", "schema"}})
			}
			res, err := storage.Migrate(ctx, o)
			if err != nil {
				return a.migrateError(err, res.Backup)
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			text := fmt.Sprintf("schema %d is current; nothing to migrate\n", res.To)
			if res.From != res.To {
				text = fmt.Sprintf("migrated schema %d → %d\n", res.From, res.To)
				if res.Backup != nil {
					text += "pre-migrate backup: " + res.Backup.Path + "\n"
				}
			}
			_, err = io.WriteString(cmd.OutOrStdout(), text)
			return err
		},
	}
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	return c
}

// migrateError renders a refused or failed migration with the way
// forward.
func (a *app) migrateError(err error, backup *storage.Backup) error {
	e := errmap.Of(err)
	switch {
	case backup != nil:
		e.SuggestedFix = "the store is still at its old schema; fix the cause and rerun `" +
			a.heoCommand("storage migrate") + "`, or if the store is unusable restore it with `" +
			a.heoCommand("storage restore "+backup.Path) + "`"
	case model.CodeOf(err) == model.SchemaMismatch:
		e.SuggestedFix = a.storageAction(storage.CompatNewer)
	}
	return e
}
