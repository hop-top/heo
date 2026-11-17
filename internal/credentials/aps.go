package credentials

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/joho/godotenv"

	kitsecret "hop.top/kit/go/storage/secret"
)

// Errors of the aps store. Fixed text: godotenv quotes the offending
// line, so its errors never pass through.
var (
	errApsMalformed  = errors.New("aps secrets file is not valid dotenv")
	errApsUnreadable = errors.New("aps secrets file is not readable")
)

// apsStore reads an aps profile's secrets file, <aps data>/profiles/
// <id>/secrets.env: the layout of aps's default "file" secrets backend,
// the file aps exports to child processes as APS_PROFILE_SECRETS, and
// the one `aps run` injects. It parses with the same dotenv library
// aps uses, rereads the file on every call (aps rotates by rewriting
// it) and is read-only: heo never writes aps state.
type apsStore struct{ path string }

var _ kitsecret.Store = apsStore{}

func (s apsStore) read() (map[string]string, error) {
	f, err := os.Open(s.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return map[string]string{}, nil
	case err != nil:
		return nil, errApsUnreadable
	}
	defer func() { _ = f.Close() }()
	m, err := godotenv.Parse(f)
	if err != nil {
		return nil, errApsMalformed
	}
	return m, nil
}

func (s apsStore) Get(ctx context.Context, key string) (*kitsecret.Secret, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := s.read()
	if err != nil {
		return nil, err
	}
	v, ok := m[key]
	if !ok {
		return nil, kitsecret.ErrNotFound
	}
	return &kitsecret.Secret{Key: key, Value: []byte(v)}, nil
}

func (s apsStore) List(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := s.read()
	if err != nil {
		return nil, err
	}
	var keys []string
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (s apsStore) Exists(ctx context.Context, key string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m, err := s.read()
	if err != nil {
		return false, err
	}
	_, ok := m[key]
	return ok, nil
}
