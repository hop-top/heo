package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// TestLostClassifiesTransport: a read or write that failed because the
// peer went away is a lost daemon, as the transport reports it; any other
// failure, and a caller that gave up, is not.
func TestLostClassifiesTransport(t *testing.T) {
	op := func(name string, errno syscall.Errno) error {
		return &net.OpError{Op: name, Net: "unix", Err: os.NewSyscallError(name, errno)}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		ctx  context.Context
		err  error
		gone bool
	}{
		"eof":              {context.Background(), io.EOF, true},
		"eof wrapped":      {context.Background(), &net.OpError{Op: "read", Err: io.EOF}, true},
		"unexpected eof":   {context.Background(), io.ErrUnexpectedEOF, true},
		"connection reset": {context.Background(), op("read", syscall.ECONNRESET), true},
		"broken pipe":      {context.Background(), op("write", syscall.EPIPE), true},
		"connection abort": {context.Background(), op("read", syscall.ECONNABORTED), true},
		"not connected":    {context.Background(), op("write", syscall.ENOTCONN), true},
		"use of closed":    {context.Background(), net.ErrClosed, true},
		"timeout":          {context.Background(), op("read", syscall.ETIMEDOUT), false},
		"not json":         {context.Background(), &json.SyntaxError{}, false},
		"something else":   {context.Background(), errors.New("boom"), false},
		"caller cancelled": {cancelled, io.EOF, false},
		"caller deadline":  {cancelled, op("read", syscall.ECONNRESET), false},
	} {
		got := Gone(Client{}.lost(tc.ctx, tc.err))
		if got != tc.gone {
			t.Errorf("%s: Gone = %v, want %v", name, got, tc.gone)
		}
	}
}
