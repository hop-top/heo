package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/cmdsurface"
	"hop.top/uma/model"
)

// rawOut is what a raw export reports once it wrote the bytes to a
// file: the message and where they went, never the bytes.
type rawOut struct {
	Message   string `json:"message"    yaml:"message"`
	MediaType string `json:"media_type" yaml:"media_type"`
	Size      int64  `json:"size"       yaml:"size"`
	Path      string `json:"path"       yaml:"path"`
}

func (a *app) messageRawCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "raw <message>",
		Short: "Export a message's RFC 5322 bytes from its provider",
		Long: `Read the provider's whole copy of the message <message> (record ID): for
mail, the RFC 5322 message with every header and part, byte for byte as
the provider serves it (media type message/rfc822), and write it where
-o says:

  -o <file>   that file
  -o <dir>    a file named after the message (<message>.eml), in that directory
  -o -        stdout: the bytes alone
  (none)      stdout

--conversation (record ID) narrows the lookup. An existing file is
never overwritten: the command exits 4 (CONFLICT) unless --force
replaces it (written whole, then renamed into place). A file is created
owner-only (0600).

The read goes to the provider each time and heo and uma keep nothing of
it. It is authorized as ` + "`message get`" + ` is, under the message's audience:
no such message, or one you may not read, exits 3. Refusals: the
channel's adapter does not declare raw_read (exit 2, UNSUPPORTED); the
provider's copy is over the adapter's size bound (exit 1,
PROVIDER_PERMANENT, reason too_large: it is never truncated); the message
was redacted or deleted, or has no provider copy (exit 4, CONFLICT); the
provider no longer holds it (exit 3). Read-only; running it twice reads
twice.

Served over REST, rpc or MCP, the result is JSON: the message, media_type,
size and content, the bytes base64.`,
		Example: `  heo message raw msg_123
  heo message raw msg_123 -o message.eml --force
  heo message raw msg_123 -o - | sha256sum`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool(flagForce)
			dest, _ := cmd.Flags().GetString(flagOutput)
			conv, _ := cmd.Flags().GetString(flagConversation)
			_, served := cmdsurface.AdmittedMeta(cmd.Context())
			if !served {
				if err := checkDestination(dest, force); err != nil {
					return err
				}
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (desk.RawMessage, error) {
				var out desk.RawMessage
				err := a.desk(ctx, c, model.ActionMessageRead, model.Audience{}, func(s desk.Service) (err error) {
					out, err = s.RawMessage(ctx, args[0], conv)
					return err
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if served {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			return a.saveRaw(cmd, res, dest, force)
		},
	}
	c.Flags().String(flagConversation, "", "Conversation the message belongs to (record ID); narrows the lookup")
	c.Flags().Bool(flagForce, false, "Replace the file if it exists")
	providerDependent(c, model.CapabilityRawRead)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &desk.RawMessage{}, "1.0")
	return c
}

// saveRaw writes res's bytes where dest says. Stdout, the default, gets
// the bytes alone; a file gets them and a summary goes to the terminal.
// As for an attachment, the summary never goes through kit's output with
// -o set: Dispatch would take -o for where to render it and truncate the
// file.
func (a *app) saveRaw(cmd *cobra.Command, res desk.RawMessage, dest string, force bool) error {
	if dest == "" || dest == toStdout {
		_, err := cmd.OutOrStdout().Write(res.Content)
		return err
	}
	path, err := target(dest, string(res.Message)+".eml")
	if err != nil {
		return err
	}
	if err := writeDownload(path, res.Content, force); err != nil {
		return err
	}
	out := rawOut{Message: string(res.Message), MediaType: res.MediaType, Size: res.Size, Path: path}
	if a.structured() {
		if f := cmd.Flags().Lookup(flagOutput); f != nil {
			_ = f.Value.Set("")
		}
		return output.Dispatch(cmd, a.r.Viper, out)
	}
	return writeRows(cmd.OutOrStdout(), [][2]string{
		{"message", out.Message},
		{"media type", out.MediaType},
		{"size", fmt.Sprint(out.Size)},
		{"path", out.Path},
	})
}
