package postgres

import (
	"errors"
	"net/url"
	"strings"
)

// hasInlinePassword reports whether dsn, a libpq URL or keyword/value
// connection string, carries a password itself. A password belongs in a
// credential reference, never in configuration.
func hasInlinePassword(dsn string) (bool, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			// url's error quotes the input, password included.
			return false, errors.New("is not a valid connection URL")
		}
		if _, set := u.User.Password(); set {
			return true, nil
		}
		return u.Query().Has("password"), nil
	}
	pairs, err := keywordPairs(dsn)
	if err != nil {
		return false, err
	}
	for _, kv := range pairs {
		if kv[0] == "password" {
			return true, nil
		}
	}
	return false, nil
}

// keywordPairs splits a libpq keyword/value string: key = value pairs
// separated by whitespace, a value single-quoted when it holds spaces,
// with backslash escaping a quote or backslash.
func keywordPairs(s string) ([][2]string, error) {
	var out [][2]string
	i := 0
	skip := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
	}
	for {
		skip()
		if i >= len(s) {
			return out, nil
		}
		start := i
		for i < len(s) && s[i] != '=' && s[i] != ' ' && s[i] != '\t' {
			i++
		}
		key := s[start:i]
		skip()
		if i >= len(s) || s[i] != '=' || key == "" {
			return nil, errors.New("is not a valid connection string: want key=value pairs")
		}
		i++ // '='
		skip()
		var val strings.Builder
		if i < len(s) && s[i] == '\'' {
			i++
			closed := false
			for i < len(s) {
				c := s[i]
				if c == '\\' && i+1 < len(s) {
					val.WriteByte(s[i+1])
					i += 2
					continue
				}
				if c == '\'' {
					i++
					closed = true
					break
				}
				val.WriteByte(c)
				i++
			}
			if !closed {
				return nil, errors.New("is not a valid connection string: unterminated quote")
			}
		} else {
			for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
				if s[i] == '\\' && i+1 < len(s) {
					val.WriteByte(s[i+1])
					i += 2
					continue
				}
				val.WriteByte(s[i])
				i++
			}
		}
		out = append(out, [2]string{key, val.String()})
	}
}

// safeParseError renders a pgx configuration or connection error. heo's
// DSN never holds a password and pgx redacts one it is given, so the
// message carries no secret.
func safeParseError(err error) string { return err.Error() }
