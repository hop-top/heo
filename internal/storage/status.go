package storage

import (
	"context"

	"hop.top/kit/go/console/cli"
)

// StatusTitle is the status section storage reports under.
const StatusTitle = "storage"

// StatusProvider returns a kit status provider for the storage section:
// driver, server, store library and schema versions and their
// compatibility. report is called on each status request; heo's service
// status feeds it from the running daemon's connection.
func StatusProvider(report func(ctx context.Context) (Report, error)) cli.StatusProvider {
	return func(ctx context.Context) (cli.StatusSection, error) {
		rep, err := report(ctx)
		if err != nil {
			return cli.StatusSection{Title: StatusTitle, Status: "error", ErrorMessage: err.Error()}, nil
		}
		return cli.StatusSection{Title: StatusTitle, Status: "ok", Data: rep}, nil
	}
}
