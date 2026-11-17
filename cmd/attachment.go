package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/cmdsurface"
	"hop.top/uma/model"
)

// Flags the attachment leaves add. -o is kit's own --output.
const (
	flagMessage = "message"
	flagForce   = "force"
	flagOutput  = "output"
	toStdout    = "-"
)

// attachmentContent is what a download reads in the daemon: the
// attachment's metadata and its bytes. The CLI writes the bytes where
// the caller asked; a served surface (REST, rpc, MCP) returns them
// as-is, base64 in JSON.
type attachmentContent struct {
	Attachment desk.Attachment `json:"attachment" yaml:"attachment"`
	Content    []byte          `json:"content"    yaml:"content"`
}

// downloadOut is what a download reports once it wrote the bytes: the
// attachment and where they went, never the bytes.
type downloadOut struct {
	Attachment string `json:"attachment" yaml:"attachment"`
	Name       string `json:"name"       yaml:"name"`
	MediaType  string `json:"media_type" yaml:"media_type"`
	Size       int64  `json:"size"       yaml:"size"`
	Digest     string `json:"digest"     yaml:"digest"`
	Path       string `json:"path"       yaml:"path"`
}

func (a *app) attachmentCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "attachment",
		Short: "List, inspect and download the attachments you may read",
		Long: `List and inspect the attachments of messages you may read, and
download their bytes. An attachment is named <inbox>/<attachment>, both
record IDs, as ` + "`attachment list`" + ` prints it.

uma decides every read against the attachment's own audience and the
messages that hold it, now: a message narrowed to internal content, or
redacted, changes what the next download does. A caller limited to
public content is refused (exit 5) once every message holding the
attachment is narrowed; an attachment whose bytes were redacted is gone
(exit 3).

Every command runs in the profile's daemon (none running exits 70) and
is open to every role that reads conversations.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.attachmentListCmd(), a.attachmentGetCmd(), a.attachmentDownloadCmd())
	return grp
}

// attachmentRow is an attachment in a listing table.
type attachmentRow struct {
	Ref        string `json:"ref"        yaml:"ref"        table:"REF"`
	Name       string `json:"name"       yaml:"name"       table:"NAME"`
	MediaType  string `json:"media_type" yaml:"media_type" table:"TYPE"`
	Size       int64  `json:"size"       yaml:"size"       table:"SIZE"`
	Visibility string `json:"visibility" yaml:"visibility" table:"VISIBILITY"`
	Messages   string `json:"messages"   yaml:"messages"   table:"MESSAGES"`
}

func attachmentRowOf(a desk.Attachment) attachmentRow {
	ms := make([]string, 0, len(a.Messages))
	for _, m := range a.Messages {
		ms = append(ms, string(m))
	}
	return attachmentRow{
		Ref: a.Ref, Name: a.Name, MediaType: a.MediaType, Size: a.Size,
		Visibility: string(a.Audience.Visibility), Messages: orDash(strings.Join(ms, ",")),
	}
}

func attachmentRows(a desk.Attachment) [][2]string {
	return [][2]string{
		{"ref", a.Ref},
		{"name", a.Name},
		{"media type", a.MediaType},
		{"size", fmt.Sprint(a.Size)},
		{"digest", orDash(a.Digest)},
		{"visibility", string(a.Audience.Visibility)},
	}
}

func (a *app) attachmentListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the attachments of a message or a conversation",
		Long: `List the attachments of the messages you may read: of --message (record
ID), or of every message of --conversation (record ID); each narrows
the other when both are given. Each row shows the attachment's ref
(what get and download take), name, media type, size, audience and the
messages that hold it. No bytes are read.

A page covers the messages uma returned for it (--limit, --cursor with
the previous page's next) and is never empty while more follow. An
attachment you may not read, or whose bytes are gone, is not listed.
Read-only.`,
		Example: `  heo attachment list --message msg_123
  heo attachment list --conversation conv_123 --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			msg, _ := cmd.Flags().GetString(flagMessage)
			conv, _ := cmd.Flags().GetString(flagConversation)
			if msg == "" && conv == "" {
				return output.UsageError("attachment list: --message or --conversation is required")
			}
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[desk.Attachment], error) {
				var out page[desk.Attachment]
				err := a.desk(ctx, c, model.ActionAttachmentRead, model.Audience{}, func(s desk.Service) error {
					p, err := s.ListAttachments(ctx, desk.Filter{Conversation: conv}, msg, pg)
					out = pageFrom(p)
					return err
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			rows := make([]attachmentRow, 0, len(res.Items))
			for _, at := range res.Items {
				rows = append(rows, attachmentRowOf(at))
			}
			return renderPage(a, cmd, rows, res.Next, res.Complete)
		},
	}
	c.Flags().String(flagMessage, "", "Message whose attachments to list (record ID)")
	c.Flags().String(flagConversation, "", "Conversation whose attachments to list (record ID)")
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[desk.Attachment]{}, "1.0")
	return c
}

func (a *app) attachmentGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <attachment>",
		Short: "Show one attachment's metadata",
		Long: `Show the metadata of the attachment <attachment> (<inbox>/<attachment>,
as ` + "`attachment list`" + ` prints it): name, media type, size, digest and
audience. No bytes are read. uma authorizes it as it does a download,
now: an attachment you may not read exits 5, one whose bytes are gone
exits 3. Read-only.`,
		Example: `  heo attachment get inb_1/att_1
  heo attachment get inb_1/att_1 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (desk.Attachment, error) {
				var out desk.Attachment
				err := a.desk(ctx, c, model.ActionAttachmentRead, model.Audience{}, func(s desk.Service) (err error) {
					out, err = s.GetAttachment(ctx, args[0])
					return err
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			return writeRows(cmd.OutOrStdout(), attachmentRows(res))
		},
	}
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &desk.Attachment{}, "1.0")
	return c
}

func (a *app) attachmentDownloadCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "download <attachment>",
		Short: "Download an attachment's bytes to a file or stdout",
		Long: `Download the attachment <attachment> (<inbox>/<attachment>, as
` + "`attachment list`" + ` prints it) and write its bytes where -o says:

  -o <file>   that file
  -o <dir>    a file named after the attachment, in that directory
  -o -        stdout: the bytes alone, the summary goes to stderr
  (none)      a file named after the attachment, in this directory

The name is the provider's, so only its last element is used. An
existing file is never overwritten: the command exits 4 (CONFLICT)
unless --force replaces it (written whole, then renamed into place). A
file is created owner-only (0600).

uma authorizes the read against the attachment's own audience and the
messages that hold it, now, and verifies the bytes against the length
and digest it recorded. A caller limited to public content is refused
(exit 5) once every message holding the attachment is narrowed; an
attachment whose bytes were redacted is gone (exit 3). The bytes never
reach a log; they cross the control socket once, to this command.
Read-only.`,
		Example: `  heo attachment download inb_1/att_1
  heo attachment download inb_1/att_1 -o invoice.pdf --force
  heo attachment download inb_1/att_1 -o - | sha256sum`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool(flagForce)
			dest, _ := cmd.Flags().GetString(flagOutput)
			_, served := cmdsurface.AdmittedMeta(cmd.Context())
			if !served {
				// Refuse before pulling the bytes where the file is known.
				if err := checkDestination(dest, force); err != nil {
					return err
				}
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (attachmentContent, error) {
				var out attachmentContent
				err := a.desk(ctx, c, model.ActionAttachmentRead, model.Audience{}, func(s desk.Service) error {
					meta, rc, err := s.OpenAttachment(ctx, args[0])
					if err != nil {
						return err
					}
					defer func() { _ = rc.Close() }()
					out.Attachment = meta
					out.Content, err = desk.ReadAll(meta, rc)
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
			return a.saveAttachment(cmd, res, dest, force)
		},
	}
	c.Flags().Bool(flagForce, false, "Replace the file if it exists")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &attachmentContent{}, "1.0")
	return c
}

// saveAttachment writes res's bytes where dest says and reports what it
// did. The summary never goes through kit's output.Dispatch with -o set:
// Dispatch would take -o for where to render the summary and truncate
// the file.
func (a *app) saveAttachment(cmd *cobra.Command, res attachmentContent, dest string, force bool) error {
	at := res.Attachment
	out := downloadOut{
		Attachment: at.Ref, Name: at.Name, MediaType: at.MediaType, Size: int64(len(res.Content)), Digest: at.Digest,
	}
	summary := cmd.OutOrStdout()
	if dest == toStdout {
		out.Path = toStdout
		if _, err := cmd.OutOrStdout().Write(res.Content); err != nil {
			return err
		}
		// The bytes own stdout: the summary is a note.
		_, err := fmt.Fprintf(cmd.ErrOrStderr(), "note: wrote %s (%d bytes) to stdout\n", out.Name, out.Size)
		return err
	}
	path, err := target(dest, at.Name)
	if err != nil {
		return err
	}
	if err := writeDownload(path, res.Content, force); err != nil {
		return err
	}
	out.Path = path
	if a.structured() {
		if f := cmd.Flags().Lookup(flagOutput); f != nil {
			_ = f.Value.Set("")
		}
		return output.Dispatch(cmd, a.r.Viper, out)
	}
	return writeRows(summary, [][2]string{
		{"attachment", out.Attachment},
		{"name", out.Name},
		{"media type", out.MediaType},
		{"size", fmt.Sprint(out.Size)},
		{"digest", orDash(out.Digest)},
		{"path", out.Path},
	})
}

// target is the file a download goes to: dest, or the file named after
// the attachment inside dest when that is a directory, or in the
// current directory when dest is empty.
func target(dest, name string) (string, error) {
	file := fileName(name)
	switch {
	case dest == "":
		return file, nil
	case isDir(dest):
		return filepath.Join(dest, file), nil
	}
	return dest, nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// fileName is the local name of a provider's attachment name: its last
// element, without control characters, never empty or a directory
// reference.
func fileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ReplaceAll(name, `\`, "/"))
	name = strings.TrimSpace(name[strings.LastIndex(name, "/")+1:])
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	return name
}

// checkDestination refuses, before any byte moves, an explicit file
// that exists and may not be replaced. A directory or stdout is fine;
// the file inside a directory is checked once the name is known.
func checkDestination(dest string, force bool) error {
	if dest == "" || dest == toStdout || force || isDir(dest) {
		return nil
	}
	if _, err := os.Lstat(dest); err == nil {
		return existsError(dest)
	}
	return nil
}

func existsError(path string) *output.Error {
	e := output.ConflictError(fmt.Sprintf("%s exists: not overwriting it", path))
	e.SuggestedFix = "pass --force to replace it, or -o to name another file"
	return e
}

// writeDownload writes b to path, owner-only. Without force it creates
// the file and fails if it exists, so a file that appears meanwhile is
// not clobbered either; with force it writes a sibling and renames it
// over path, so the old file is replaced whole or not at all.
func writeDownload(path string, b []byte, force bool) error {
	if !force {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the caller named the file
		if errors.Is(err, fs.ErrExist) {
			return existsError(path)
		}
		if err != nil {
			return err
		}
		if _, err := f.Write(b); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return err
		}
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".heo-download-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}
