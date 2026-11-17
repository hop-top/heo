package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/trustcfg"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

const (
	flagTrustFile = "file"
	// trustImportReadLimit bounds the import file: JSON of counts only.
	trustImportReadLimit = 4 << 20
	// codeTrustImportFailed is the error code of an import that refused
	// or failed an entry: the report is printed first.
	codeTrustImportFailed = "VALIDATION"
)

// readTrustImportInput reads --file for a command typed at a terminal:
// "-" is standard input, a value that is a JSON object is the file
// itself, anything else a path. At most trustImportReadLimit bytes.
func readTrustImportInput(cmd *cobra.Command, name string) ([]byte, error) {
	var r io.Reader
	switch trimmed := strings.TrimSpace(name); {
	case trimmed == "":
		return nil, output.UsageError("--" + flagTrustFile + ": name the file holding the trust to import, or - for standard input")
	case strings.HasPrefix(trimmed, "{"):
		return []byte(trimmed), nil
	case trimmed == "-":
		r = cmd.InOrStdin()
	default:
		f, err := os.Open(name) //nolint:gosec // G304: the caller named the file
		if err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			return nil, output.UsageError(fmt.Sprintf("--%s %s: %v", flagTrustFile, name, err))
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, trustImportReadLimit+1))
	if err != nil {
		return nil, output.UsageError(fmt.Sprintf("--%s: %v", flagTrustFile, err))
	}
	if len(b) > trustImportReadLimit {
		return nil, output.UsageError(fmt.Sprintf("--%s: larger than %d MiB", flagTrustFile, trustImportReadLimit>>20))
	}
	return b, nil
}

// trustImportFile returns the file the leaf was given, parsed. At the
// terminal --file names a file or standard input, which the daemon cannot
// see: this process reads and checks it and sends its JSON in the flag's
// place. The daemon, and a served surface (MCP, REST, gRPC), takes the
// flag as the JSON itself.
func (a *app) trustImportFile(cmd *cobra.Command) (desk.TrustImportFile, error) {
	route, _ := control.RouteOf(cmd.Context(), a.control.servesControl())
	raw, _ := cmd.Flags().GetString(flagTrustFile)
	if route == control.Forward {
		b, err := readTrustImportInput(cmd, raw)
		if err != nil {
			return desk.TrustImportFile{}, err
		}
		raw = string(b)
	} else if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return desk.TrustImportFile{}, output.UsageError("--" + flagTrustFile +
			": the daemon cannot read your files; pass the import file's JSON object as the value")
	}
	f, err := desk.ParseTrustImport([]byte(raw))
	if err != nil {
		return desk.TrustImportFile{}, output.UsageError(strings.TrimPrefix(err.Error(), "validation: "))
	}
	if err := cmd.Flags().Set(flagTrustFile, raw); err != nil {
		return desk.TrustImportFile{}, err
	}
	return f, nil
}

// trustImportRow is an entry's outcome in a table.
type trustImportRow struct {
	Index     int    `json:"index"     yaml:"index"     table:"#"`
	Status    string `json:"status"    yaml:"status"    table:"STATUS"`
	Key       string `json:"key"       yaml:"key"       table:"KEY"`
	Clean     int64  `json:"clean"     yaml:"clean"     table:"CLEAN"`
	Automatic string `json:"automatic" yaml:"automatic" table:"AUTOMATIC"`
	Note      string `json:"note"      yaml:"note"      table:"NOTE"`
}

func trustImportRowOf(r desk.TrustImportRow) trustImportRow {
	note := r.Reason
	switch {
	case note != "":
	case r.BecomesAutomatic:
		note = "becomes automatic"
	case !r.Automatic:
		note = r.HoldReason
	}
	key := r.Key
	if key == "" {
		key = "-"
	}
	return trustImportRow{Index: r.Index, Status: r.Status, Key: key, Clean: r.Clean, Automatic: yesNo(r.Automatic), Note: note}
}

func (a *app) trustImportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "import",
		Short: "Seed trust keys from the approval history a deployment earned elsewhere",
		Long: `Import counts of clean approvals into uma's trust counters, so a
mailbox's plugins keep the trust they earned before. --file names a JSON
file, - reads standard input, and a value that is itself a JSON object is
the file (over MCP, REST and gRPC, which cannot read your files, pass the
object). The file holds counts only, no message data:

  {"version": 1, "entries": [{"channel": "support", "proposer": "plugin:triage",
    "on_behalf_of": "uid:1001", "verb": "state", "class": "archive",
    "clean": 12, "last_correction_at": "2026-10-02T09:30:00Z"}]}

channel is an inbox key or ID; verb and class are the held verb and its
target as trust list shows them (reply:send, note:local or note:provider,
state:<state>, move:<class>, label:<name>, unlabel:<name>, tag:<name>, start, and edit,
delete and react on message); on_behalf_of, class (where the verb takes
none) and last_correction_at are optional. A field outside the schema, a
version other than 1, an unknown inbox or verb, a proposer that is not a
plugin: actor and two entries for one key are refused. Every entry is
checked before any is written: one refusal refuses the file and nothing
is written.

uma merges and never replaces: each count becomes the larger of the stored
and the imported one, so importing a file again changes nothing and an
import never lowers the trust live decisions earned. The consecutive clean
count is raised only while no stored correction is later than the file's.
uma writes key by key; a write that fails part way leaves the earlier keys
imported, and the report says which.

--dry-run writes nothing: it reports each key as the import would leave
it and, under the current trust configuration, which keys would become
automatic. The result gives the entries imported, unchanged, refused and
failed, each with its reason; --format json has the counts and the
per-entry detail. Exits 2 when an entry is refused or failed, after
printing the report. Operator role (trust.write); a plugin actor never
holds it.`,
		Example: `  heo operation trust import --file trust.json --dry-run
  heo operation trust import --file trust.json
  heo operation trust import --file trust.json --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := a.trustImportFile(cmd)
			if err != nil {
				return err
			}
			dry := cli.IsDryRun(cmd)
			cfg, err := trustcfg.Load(a.r.Viper)
			if err != nil {
				return err
			}
			res, err := daemonRunReport(a, cmd, args, codeTrustImportFailed, func(ctx context.Context, c control.Caller) (desk.TrustImportResult, error) {
				var out desk.TrustImportResult
				err := a.desk(ctx, c, model.ActionTrustWrite, model.Audience{}, func(s desk.Service) error {
					return a.runtime.Use(ctx, func(rt daemon.Runtime) error {
						svc, err := rt.Engine.For(model.Principal{Tenant: c.Tenant, Actor: daemon.ServiceActor})
						if err != nil {
							return err
						}
						out, err = s.ImportTrust(ctx, f, desk.TrustImportOptions{DryRun: dry, Config: cfg, Now: time.Now(), Read: svc.Operations()})
						return err
					})
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				err = output.Dispatch(cmd, a.r.Viper, res)
			} else {
				err = a.writeTrustImport(cmd, res)
			}
			if err != nil {
				return err
			}
			return trustImportFailure(res)
		},
	}
	c.Flags().String(flagTrustFile, "", "JSON file of the trust to import, - for standard input, or the JSON object itself (required)")
	_ = c.MarkFlagRequired(flagTrustFile)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &desk.TrustImportResult{}, Version: "1.0"})
	return c
}

func (a *app) writeTrustImport(cmd *cobra.Command, res desk.TrustImportResult) error {
	rows := make([]trustImportRow, len(res.Entries))
	for i, r := range res.Entries {
		rows[i] = trustImportRowOf(r)
	}
	if err := output.Dispatch(cmd, a.r.Viper, rows); err != nil {
		return err
	}
	prefix := ""
	if res.DryRun {
		prefix = "dry run, nothing written: "
	}
	_, err := fmt.Fprintf(cmd.ErrOrStderr(), "%s%d imported, %d unchanged, %d refused, %d failed; %d becoming automatic\n",
		prefix, res.Imported, res.Unchanged, res.Refused, res.Failed, res.Automatic)
	return err
}

// trustImportFailure is the exit of an import: nil when every entry was
// imported or already so.
func trustImportFailure(res desk.TrustImportResult) error {
	var msg, fix string
	switch {
	case res.Refused > 0:
		msg, fix = fmt.Sprintf("%d of %d entries refused: nothing was written", res.Refused, len(res.Entries)), "fix the entries named above, then import again"
	case res.Failed > 0:
		msg, fix = fmt.Sprintf("%d entries failed to import; the keys before it are imported", res.Failed), "import again: the keys already imported stay as they are"
	default:
		return nil
	}
	e := output.UsageError(msg)
	e.Code, e.SuggestedFix = codeTrustImportFailed, fix
	return e
}
