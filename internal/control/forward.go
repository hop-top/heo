package control

import (
	"encoding/csv"
	"encoding/json"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/cmdsurface"
	"hop.top/kit/go/transport/socket"
)

// formatFlag is kit's output format flag. A forwarded command always
// asks the daemon for json: its data and its error envelope come back
// structured, and the client renders them in the caller's own format.
const formatFlag = "format"

// Kit's confirmation and preview flags.
const (
	confirmFlag = "confirm"
	dryRunFlag  = "dry-run"
)

// SocketPolicy is the control socket's destructive ceiling. The socket
// is the CLI's only path to the daemon, so a destructive command the
// operator confirmed at the terminal must be able to run there: the
// socket, and only the socket, is named. REST, gRPC and MCP keep kit's
// default and refuse destructive commands.
func SocketPolicy() cmdsurface.Policy {
	return cmdsurface.Policy{AllowDestructiveOn: []cmdsurface.Surface{cmdsurface.SurfaceSocket}}
}

// RequestFor builds the socket request that runs cmd with args in the
// daemon: its path below the root, its positional arguments, and the
// flags set on the command line that cmd itself declares. Root flags
// stay in this process: they shape how the answer is rendered here, or
// select the profile, which the daemon serves already. Two travel:
//
//   - --dry-run, so the daemon previews rather than acts;
//   - the confirmation of a destructive command. Kit's gate asked for
//     it here, at the operator's terminal, before the command ran; a
//     served call has no terminal, so the outcome is sent as
//     confirm=yes. A refused confirmation never gets this far.
func RequestFor(cmd *cobra.Command, args []string) socket.Request {
	path := strings.Fields(cmd.CommandPath())
	if len(path) > 0 {
		path = path[1:]
	}
	flags := map[string]any{}
	cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		if !f.Changed {
			return
		}
		switch v := f.Value.(type) {
		case pflag.SliceValue:
			flags[f.Name] = sliceFlag(f.Value.Type(), v.GetSlice())
		default:
			// A bool travels as its text too: the served call's argv
			// drops a false bool, so --flag=false would arrive unset.
			flags[f.Name] = f.Value.String()
		}
	})
	flags[formatFlag] = output.JSON
	if f := cmd.Flags().Lookup(dryRunFlag); f != nil && f.Changed {
		flags[dryRunFlag] = f.Value.String() == "true"
	}
	if se, ok := cli.GetSideEffect(cmd); ok && strings.HasPrefix(string(se), string(cli.SideEffectDestructive)) {
		flags[confirmFlag] = "yes"
	}
	if args == nil {
		args = []string{}
	}
	return socket.Request{Path: path, Args: args, Flags: flags}
}

// Decode turns the daemon's result into the command's output or its
// error, exactly as the command produced them: a non-zero exit becomes
// the error envelope the command rendered, with its code, exit code,
// fix and transience, for this process to render in its own format.
func Decode[T any](res Result) (T, error) {
	var out T
	if res.ExitCode != 0 {
		return out, envelopeOf(res)
	}
	data := res.Data
	if len(data) == 0 {
		data = json.RawMessage(res.Stdout)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, &output.Error{Code: output.CodeGeneric, ExitCode: output.ExitGeneric,
			Message:      "the daemon's answer does not decode: " + err.Error(),
			Transience:   output.TransienceUnknown,
			SuggestedFix: "the daemon may run another heo version: restart it on this one"}
	}
	return out, nil
}

// DecodeReport is Decode for a command that renders a report and then
// fails with code when the report holds a failure, as a check does: such
// a failure comes back as the report, for this process to render and
// judge in turn. Any other failure, and one without a report, is the
// error, as Decode returns it.
func DecodeReport[T any](res Result, code string) (T, error) {
	if res.ExitCode != 0 && len(res.Data) > 0 && envelopeOf(res).Code == code {
		var out T
		if err := json.Unmarshal(res.Data, &out); err == nil {
			return out, nil
		}
	}
	return Decode[T](res)
}

// envelopeOf recovers the error envelope a failed command wrote to its
// stderr. A stderr that holds none is carried as the message, with the
// command's exit code.
func envelopeOf(res Result) *output.Error {
	var e output.Error
	dec := json.NewDecoder(strings.NewReader(res.Stderr))
	if err := dec.Decode(&e); err == nil && e.Code != "" {
		if e.ExitCode == 0 {
			e.ExitCode = res.ExitCode
		}
		return &e
	}
	msg := strings.TrimSpace(res.Stderr)
	if msg == "" {
		msg = "the command failed in the daemon"
	}
	return &output.Error{Code: output.CodeGeneric, Message: msg, ExitCode: res.ExitCode,
		Transience: output.TransienceUnknown}
}

// sliceFlag is how a repeated flag travels, so the daemon's own parse of
// it gives back the values the caller set. A string array takes each value
// whole, so it travels as the list. A string slice reads its value as CSV,
// so it travels as CSV of its values: an empty value, or one holding a
// comma or a quote, survives. Any other slice joins with commas, as its
// values hold none. Set to nothing (--flag=) a flag travels as "": the
// served argv drops an empty list.
func sliceFlag(typ string, vs []string) any {
	if len(vs) == 0 {
		return ""
	}
	switch typ {
	case "stringArray":
		return vs
	case "stringSlice":
		var b strings.Builder
		w := csv.NewWriter(&b)
		_ = w.Write(vs)
		w.Flush()
		return strings.TrimSuffix(b.String(), "\n")
	}
	return strings.Join(vs, ",")
}
