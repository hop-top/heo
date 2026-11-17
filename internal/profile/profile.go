// Package profile resolves heo profile references.
//
// A profile is one heo runtime: one config file set, one data store,
// one control socket, one tenant. It is named on the command line with
// --profile <ref> or through HEO_PROFILE, where <ref> is a URI in the
// hop.top/cite grammar:
//
//	heo://profile/<name>   a native heo profile
//	aps://profile/<id>     a heo profile bound to an aps profile
//
// Two shortcuts expand before parsing: a bare <name> (no colon) means
// heo://profile/<name>, and aps:<id> means aps://profile/<id>. Any
// other scheme is a usage error that lists the supported ones.
//
// Each profile keeps its state under a directory name, Dir: the heo
// name for a native profile, aps-<id> for an aps-bound one, so the two
// namespaces never share a directory.
package profile

import (
	"fmt"
	"regexp"
	"strings"

	"hop.top/cite/scheme"
	"hop.top/kit/go/console/output"
)

// Namespace is the cite namespace every profile reference carries.
const Namespace = "profile"

// Scheme names.
const (
	// HeoScheme marks a native heo profile.
	HeoScheme = "heo"
	// ApsScheme marks a profile bound to an aps profile. It equals
	// aps's own canonical scheme for profile URIs.
	ApsScheme = "aps"
	// ApsSpace is aps's canonical space for profile URIs. heo's
	// Namespace must stay equal to it for aps references to round-trip.
	ApsSpace = "profile"
)

// DefaultName is the native profile used when no reference is given.
const DefaultName = "default"

// apsDirPrefix prefixes the directory of every aps-bound profile. A
// native name may not start with it.
const apsDirPrefix = ApsScheme + "-"

var (
	// nameRe bounds a native heo profile name: it becomes a directory
	// name inside the socket path, whose byte budget allows 16.
	nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)
	// apsIDRe bounds an aps profile id on heo's side: aps does not
	// validate ids, and heo turns the id into a path segment.
	apsIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,15}$`)
)

// Ref is a resolved profile reference.
type Ref struct {
	// Scheme is HeoScheme or ApsScheme.
	Scheme string
	// ID is the heo profile name or the aps profile id.
	ID string
}

// Default returns the reference used when none is given.
func Default() Ref { return Ref{Scheme: HeoScheme, ID: DefaultName} }

// String returns the canonical URI, e.g. heo://profile/default.
func (r Ref) String() string {
	u := scheme.URI{Scheme: r.Scheme, Namespace: Namespace, ID: r.ID}
	return u.String()
}

// Dir returns the directory name that holds the profile's config,
// data, state and socket: the name itself for a native profile,
// aps-<id> for an aps-bound one.
func (r Ref) Dir() string {
	if r.Scheme == ApsScheme {
		return apsDirPrefix + r.ID
	}
	return r.ID
}

// IsAps reports whether the reference binds an aps profile.
func (r Ref) IsAps() bool { return r.Scheme == ApsScheme }

// registry dispatches parsing by scheme: one registration per
// supported scheme, so an unregistered one fails and Types lists the
// supported set.
var registry = newRegistry()

func newRegistry() *scheme.Registry {
	reg := scheme.NewRegistry()
	for _, name := range []string{HeoScheme, ApsScheme} {
		if err := reg.Register(scheme.TypeRegistration{Name: name}); err != nil {
			panic(err)
		}
	}
	return reg
}

// Schemes returns the supported schemes in sorted order.
func Schemes() []string { return registry.Types() }

// Parse resolves a --profile / HEO_PROFILE value. An empty value is the
// default profile. Every malformed reference is a USAGE error (exit 2).
func Parse(input string) (Ref, error) {
	if input == "" {
		return Default(), nil
	}
	expanded := expand(input)
	u, err := registry.Parse(expanded)
	if err != nil {
		if s := schemeOf(expanded); s != "" && !supported(s) {
			return Ref{}, usage(fmt.Sprintf("unsupported profile scheme %q; supported: %s",
				s, strings.Join(Schemes(), ", ")), input)
		}
		return Ref{}, usage(fmt.Sprintf("invalid profile reference %q: %v", input, err), input)
	}
	return check(input, u)
}

// expand turns the shortcuts into full URIs: <name> into
// heo://profile/<name>, aps:<id> into aps://profile/<id>. Anything else
// is returned unchanged for cite to parse.
func expand(input string) string {
	if !strings.Contains(input, ":") {
		return HeoScheme + "://" + Namespace + "/" + input
	}
	if rest, ok := cutPrefixFold(input, ApsScheme+":"); ok && !strings.HasPrefix(rest, "//") {
		return ApsScheme + "://" + Namespace + "/" + rest
	}
	return input
}

// check applies what cite does not: the namespace, a single ID
// segment, no query or fragment, and the per-scheme ID charset.
func check(input string, u *scheme.URI) (Ref, error) {
	s := strings.ToLower(u.Scheme)
	if u.Namespace != Namespace {
		return Ref{}, usage(fmt.Sprintf("invalid profile reference %q: namespace must be %q, got %q",
			input, Namespace, u.Namespace), input)
	}
	if u.Query != "" || u.Fragment != "" || strings.ContainsAny(input, "?#") {
		return Ref{}, usage(fmt.Sprintf("invalid profile reference %q: query and fragment are not allowed", input), input)
	}
	// cite joins extra path segments and decodes %2F into the ID; either
	// way the ID would no longer be one path segment.
	if strings.Contains(u.ID, "/") || strings.Contains(strings.ToLower(input), "%2f") {
		return Ref{}, usage(fmt.Sprintf("invalid profile reference %q: the id must be a single segment", input), input)
	}
	switch s {
	case HeoScheme:
		if err := checkName(u.ID); err != nil {
			return Ref{}, usage(fmt.Sprintf("invalid profile reference %q: %v", input, err), input)
		}
	case ApsScheme:
		if !apsIDRe.MatchString(u.ID) {
			return Ref{}, usage(fmt.Sprintf(
				"invalid profile reference %q: aps profile id must match %s", input, apsIDRe), input)
		}
	}
	return Ref{Scheme: s, ID: u.ID}, nil
}

// checkName validates a native heo profile name.
func checkName(name string) error {
	if strings.HasPrefix(name, apsDirPrefix) {
		return fmt.Errorf("a heo profile name may not start with %q (reserved for aps-bound profiles; use aps:%s)",
			apsDirPrefix, strings.TrimPrefix(name, apsDirPrefix))
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("heo profile name must match %s", nameRe)
	}
	return nil
}

func supported(s string) bool {
	for _, t := range Schemes() {
		if t == s {
			return true
		}
	}
	return false
}

// schemeRe is RFC 3986's scheme grammar.
var schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)

// schemeOf returns the lower-cased scheme of a URI-shaped input
// (<scheme>://... or <scheme>:...), or "" when it has none.
func schemeOf(input string) string {
	i := strings.Index(input, ":")
	if i <= 0 || !schemeRe.MatchString(input[:i]) {
		return ""
	}
	return strings.ToLower(input[:i])
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

func usage(msg, input string) *output.Error {
	e := output.UsageError(msg)
	e.SuggestedFix = fmt.Sprintf("pass --profile <name>, heo://profile/<name>, aps:<id> or aps://profile/<id> (got %q)", input)
	return e
}
