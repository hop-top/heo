package profile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"hop.top/kit/go/console/output"
)

// apsDataDir mirrors aps's own data directory resolution:
// $APS_DATA_PATH, then $XDG_DATA_HOME/aps, then ~/.local/share/aps on
// every platform. heo only reads under it and never writes aps state.
func apsDataDir() (string, error) {
	if p := os.Getenv("APS_DATA_PATH"); p != "" {
		return p, nil
	}
	if p := os.Getenv("XDG_DATA_HOME"); p != "" {
		return filepath.Join(p, "aps"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "aps"), nil
}

// ApsProfilePath returns where aps keeps the definition of profile id:
// <aps data>/profiles/<id>/profile.yaml.
func ApsProfilePath(id string) (string, error) {
	dir, err := apsDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "profiles", id, "profile.yaml"), nil
}

// ApsSecretsPath returns the secrets file of aps profile id, the
// layout of aps's default secrets backend:
// <aps data>/profiles/<id>/secrets.env.
func ApsSecretsPath(id string) (string, error) {
	dir, err := apsDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "profiles", id, "secrets.env"), nil
}

// CheckExists verifies that an aps-bound reference names an existing
// aps profile. A native reference always passes. A missing aps
// profile is NOT_FOUND (exit 3) naming the reference.
func CheckExists(r Ref) error {
	if !r.IsAps() {
		return nil
	}
	path, err := ApsProfilePath(r.ID)
	if err != nil {
		return fmt.Errorf("locate aps profile %s: %w", r, err)
	}
	info, err := os.Stat(path)
	switch {
	case err == nil && info.Mode().IsRegular():
		return nil
	case err == nil, errors.Is(err, fs.ErrNotExist):
		e := output.NotFoundError(fmt.Sprintf("aps profile %s not found (no %s)", r, path))
		e.SuggestedFix = fmt.Sprintf("create it with `aps profile create %s`, or pick an existing one from `aps profile list`", r.ID)
		return e
	default:
		return fmt.Errorf("check aps profile %s: %w", r, err)
	}
}
