package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gofrs/flock"
)

// Accounts claims provider accounts on this host, across every profile:
// one lock file per account under one directory every profile shares.
// The lock is an OS file lock, so a crashed daemon's claims go with its
// process. File names are hashes: the account itself is never written
// to disk.
type Accounts struct {
	dir string
}

// NewAccounts claims accounts under dir, created owner-only on first
// use.
func NewAccounts(dir string) *Accounts { return &Accounts{dir: dir} }

// Claim is a held account claim.
type Claim struct {
	lock *flock.Flock
	once sync.Once
}

// Claim takes account for holder, a description such as
// "heo/default (pid 123)" another daemon's refusal names. A claim
// another process holds is refused with its holder's description.
func (a *Accounts) Claim(account, holder string) (*Claim, error) {
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return nil, fmt.Errorf("account claims: %w", err)
	}
	sum := sha256.Sum256([]byte(account))
	path := filepath.Join(a.dir, hex.EncodeToString(sum[:16])+".lock")
	l := flock.New(path, flock.SetPermissions(0o600))
	ok, err := l.TryLock()
	if err != nil {
		return nil, fmt.Errorf("account claims: %w", err)
	}
	if !ok {
		who, _ := os.ReadFile(path) //nolint:gosec // path is built from a hash under the claims directory
		desc := strings.TrimSpace(string(who))
		if desc == "" {
			desc = "another heo daemon"
		}
		return nil, &claimedError{holder: desc}
	}
	// The holder line is diagnostics for a refused claimant; the lock,
	// not the content, is the claim.
	_ = os.WriteFile(path, []byte(holder+"\n"), 0o600)
	return &Claim{lock: l}, nil
}

// Release gives the claim up. It is idempotent.
func (c *Claim) Release() {
	if c == nil {
		return
	}
	c.once.Do(func() { _ = c.lock.Unlock() })
}

// claimedError is a claim another process holds.
type claimedError struct{ holder string }

func (e *claimedError) Error() string { return "received by " + e.holder }
