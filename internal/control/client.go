// Package control is heo's local control surface: the client the CLI
// sends daemon-backed commands through, and the rules every served
// surface applies to them.
//
// The CLI never touches the profile's data store. A command that needs
// the runtime runs in the daemon, the one process that owns the store;
// everywhere else it is sent over the profile's control socket as one
// request of kit's socket wire protocol, and the daemon's answer is
// rendered unchanged. No command starts a daemon: with none running
// the client fails with PREREQUISITE (exit 70) naming the command that
// starts one.
//
// Who a command acts for is decided by the daemon, never by the
// request: the socket's peer credentials (uid:<n>), or the credential
// an HTTP surface verified. A caller or tenant field a request carries
// is ignored.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"hop.top/heo/internal/errmap"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/socket"
)

// dialTimeout bounds connecting to the socket; the command itself runs
// as long as the caller's context allows.
const dialTimeout = 5 * time.Second

// Client sends commands to one profile's daemon over its control
// socket. Its zero value is not usable: Path is required.
type Client struct {
	// Path is the control socket.
	Path string
	// Start is the command line that starts the daemon, named in the
	// error when none answers, such as "heo service start".
	Start string
}

// Result is what the daemon answered for a command that ran: its exit
// code, its streams, and the structured output of a command that
// declares an output schema.
type Result struct {
	ExitCode int             `json:"exit_code"`
	Stdout   string          `json:"stdout,omitempty"`
	Stderr   string          `json:"stderr,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// reply is one response line: kit's socket wire response, with the
// result's data kept as raw JSON so numbers survive exactly.
type reply struct {
	Ok     bool          `json:"ok"`
	Result *Result       `json:"result,omitempty"`
	Error  *socket.Error `json:"error,omitempty"`
}

// Invoke checks the socket is the caller's own, sends req and returns
// the daemon's result. Every failure is a kit structured error:
// PREREQUISITE when no daemon answers, UNAUTHORIZED when the socket or
// its directory is not the caller's own or the daemon refused the
// request, and the refusal's class otherwise (see errmap.Wire).
//
// req is sent as given except for its caller and tenant, which are
// cleared: the daemon derives both and would ignore them anyway.
func (c Client) Invoke(ctx context.Context, req socket.Request) (Result, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	req.Caller, req.Tenant = "", ""
	line, err := json.Marshal(req)
	if err != nil {
		return Result{}, errmap.Of(err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return Result{}, c.lost(ctx, err)
	}
	var r reply
	if err := json.NewDecoder(conn).Decode(&r); err != nil {
		return Result{}, c.lost(ctx, err)
	}
	switch {
	case !r.Ok && r.Error != nil:
		e := errmap.Wire(*r.Error)
		if shuttingDown(*r.Error) {
			return Result{}, c.gone(e, e.Message)
		}
		return Result{}, e
	case r.Ok && r.Result != nil:
		return *r.Result, nil
	default:
		return Result{}, &output.Error{Code: output.CodeGeneric, ExitCode: output.ExitGeneric,
			Message: "the daemon answered with neither a result nor an error", Transience: output.TransienceUnknown}
	}
}

// Check verifies a daemon answers on the socket, without sending a
// command: the socket is the caller's own and something accepts a
// connection on it. It fails as Invoke does.
func (c Client) Check(ctx context.Context) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	return conn.Close()
}

// dial checks the socket is the caller's own, then connects to it.
func (c Client) dial(ctx context.Context) (net.Conn, error) {
	if err := CheckSocket(c.Path); err != nil {
		return nil, c.refusal(err)
	}
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "unix", c.Path)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
			return nil, c.notRunning(err)
		}
		return nil, &output.Error{Code: output.CodeGeneric, ExitCode: output.ExitGeneric,
			Message:    fmt.Sprintf("cannot reach the daemon on %s: %v", c.Path, err),
			Transience: output.TransienceUnknown}
	}
	return conn, nil
}

// notRunning is the corrective error for a socket nothing listens on.
func (c Client) notRunning(cause error) *output.Error {
	e := output.PrerequisiteError(fmt.Sprintf("no heo daemon is running: nothing listens on %s", c.Path))
	e.Cause = cause.Error()
	e.SuggestedFix = "start it with `" + c.start() + "`, then rerun"
	return e
}

func (c Client) start() string {
	if c.Start == "" {
		return "heo service start"
	}
	return c.Start
}

// lost reports a connection that ended before the daemon answered. A
// command that changes state may have run: the outcome is unknown. A
// connection the daemon closed or reset is the daemon going away (see
// Gone); anything else on the wire is the plain failure it is.
func (c Client) lost(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errmap.Of(ctx.Err())
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		msg := "the daemon closed the connection before answering"
		return c.gone(c.failure(msg), msg)
	}
	if transportLost(err) {
		return c.gone(c.failure(err.Error()), err.Error())
	}
	return c.failure(err.Error())
}

// failure is a connection failure whose outcome is unknown.
func (c Client) failure(msg string) *output.Error {
	return &output.Error{Code: output.CodeGeneric, ExitCode: output.ExitGeneric,
		Message: msg, Transience: output.TransienceUnknown,
		SuggestedFix: "check the daemon with `heo service status`"}
}

// transportLost reports a write or read that failed because the peer
// went away: the connection was reset, broken, closed or no longer
// connected under us (a write on a socket the peer already shut down can
// meet ENOTCONN where it usually meets EPIPE).
func transportLost(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ENOTCONN) ||
		errors.Is(err, net.ErrClosed)
}

// shuttingDown reports the daemon's answer to a call it cancelled
// because it is stopping: kit's socket reports the cancellation of a
// call as INTERNAL, carrying the context error. This client never
// cancels a call without closing its connection, so a cancellation the
// daemon reports is the daemon's own.
func shuttingDown(e socket.Error) bool {
	return e.Code == socket.CodeInternal && strings.Contains(e.Message, "canceled: "+context.Canceled.Error())
}

// goneError marks a failure as the daemon having stopped answering a
// call it had accepted: a connection it closed or reset, or a call it
// cancelled as it stopped. Unlike a refusal, the daemon may still come
// back, and a call that only reads can be sent again.
type goneError struct {
	msg   string
	start string
}

func (e *goneError) Error() string { return e.msg }

// gone marks e as a lost daemon.
func (c Client) gone(e *output.Error, msg string) *output.Error {
	return e.Retaining(&goneError{msg: msg, start: c.start()})
}

// Gone reports whether err is the daemon having stopped answering a call
// it had accepted: its connection closed or reset mid-call, or the call
// cancelled as the daemon stops. It is not set for a daemon that answered
// with an error of its own, nor for one that was never reached (that is
// PREREQUISITE already). The error keeps its own class, unknown outcome,
// for a command that may have changed state; a watch, whose steps are
// safe to send again, reads it as the daemon being unavailable
// (see Unavailable).
func Gone(err error) bool {
	var g *goneError
	return errors.As(err, &g)
}

// Unavailable renders a Gone err as what a daemon that is not there is,
// PREREQUISITE (exit 70) naming the command that starts one; any other
// err comes back as errmap.Of renders it.
func Unavailable(err error) *output.Error {
	var g *goneError
	if !errors.As(err, &g) {
		return errmap.Of(err)
	}
	e := output.PrerequisiteError("the daemon stopped answering: " + g.msg)
	e.SuggestedFix = "start it with `" + g.start + "`, then rerun"
	return e
}

// refusal renders a failed socket check.
func (c Client) refusal(err error) *output.Error {
	var nr *notRunningError
	if errors.As(err, &nr) {
		return c.notRunning(nr.cause)
	}
	var oe *ownerError
	if errors.As(err, &oe) {
		e := output.UnauthorizedError(oe.Error())
		e.SuggestedFix = oe.fix
		return e
	}
	return errmap.Of(err)
}

// notRunningError is a socket, or its directory, that does not exist.
type notRunningError struct{ cause error }

func (e *notRunningError) Error() string { return e.cause.Error() }

// ownerError is a socket or directory the caller must not trust.
type ownerError struct {
	msg string
	fix string
}

func (e *ownerError) Error() string { return e.msg }

// CheckSocket verifies path is a Unix socket the caller owns, in a
// directory the caller owns that no other user can enter or write: a
// socket another user placed there, or could replace, would receive
// every command and answer as if it were the daemon. Symbolic links
// are not followed. A missing socket or directory is reported as no
// daemon running.
func CheckSocket(path string) error {
	if path == "" {
		return &notRunningError{cause: errors.New("no control socket path configured")}
	}
	dir := filepath.Dir(path)
	di, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return &notRunningError{cause: err}
		}
		return err
	}
	if !di.IsDir() {
		return &ownerError{
			msg: fmt.Sprintf("control socket directory %s is not a directory", dir),
			fix: "remove it, or set services.socket.path to a socket in a directory you own",
		}
	}
	if err := checkOwner(dir, di, true); err != nil {
		return err
	}
	si, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &notRunningError{cause: err}
		}
		return err
	}
	if si.Mode()&os.ModeSocket == 0 {
		return &ownerError{
			msg: fmt.Sprintf("control socket %s exists and is not a socket", path),
			fix: "remove it, then start the daemon",
		}
	}
	return checkOwner(path, si, false)
}
