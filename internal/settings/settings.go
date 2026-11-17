// Package settings loads heo's own configuration for one profile and
// reports each effective value with the layer it came from.
//
// Effective order, lowest to highest:
//
//  1. built-in defaults (Defaults)
//  2. system  /etc/heo/config.yaml
//  3. user    <user config dir>/heo/config.yaml
//  4. profile <user config dir>/heo/profiles/<pdir>.yaml
//  5. -c <path> files, in order
//  6. environment, HEO_<KEY> with dots as underscores
//  7. command-line flags bound to the key
//  8. -c key=value overrides
//
// heo reads only these files: a repository's generic kit project files
// (kit.yaml, .kit.yaml, .kit/config.yaml) never configure it, and the
// result never depends on the working directory. The profile itself is
// chosen before any file loads, from --profile or HEO_PROFILE only, so a
// profile key in a file or a -c override is refused.
package settings

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/viper"

	"hop.top/heo/internal/profile"
	"hop.top/kit/go/console/output"
	kitconfig "hop.top/kit/go/core/config"
	"hop.top/kit/go/core/xdg"
)

// Tool is heo's name in config and XDG paths.
const Tool = "heo"

// Origin names the layer an effective value came from.
type Origin string

// Origins, lowest precedence first.
const (
	OriginDefault  Origin = "default"
	OriginSystem   Origin = "system"
	OriginUser     Origin = "user"
	OriginProfile  Origin = "profile"
	OriginFile     Origin = "file"
	OriginEnv      Origin = "env"
	OriginFlag     Origin = "flag"
	OriginOverride Origin = "override"
)

// OriginStore marks a finding about configuration held in the profile's
// data store — channels and inboxes — rather than in a settings layer.
const OriginStore Origin = "store"

// Inputs selects what Load layers.
type Inputs struct {
	// Profile is the resolved profile.
	Profile profile.Ref
	// ProfileOrigin and ProfileDetail record where Profile came from:
	// OriginFlag ("--profile"), OriginEnv ("HEO_PROFILE") or
	// OriginDefault.
	ProfileOrigin Origin
	ProfileDetail string
	// SystemPath, UserPath and ProfilePath are the conventional files;
	// Paths fills them for a profile.
	SystemPath  string
	UserPath    string
	ProfilePath string
	// Files are the -c <path> files, in order. Each must exist.
	Files []string
	// Overrides are the -c key=value pairs, as kit parses them: one
	// nested map.
	Overrides map[string]any
	// RequireProfileFile makes a missing profile file for a named
	// native profile NOT_FOUND. The default and aps-bound profiles
	// never need one.
	RequireProfileFile bool
	// EnvPrefix is the environment prefix BindEnv was given (HEO).
	EnvPrefix string
	// Flags reports the command-line flag bound to a key, if one was
	// set; nil means none.
	Flags func(key string) (name string, changed bool)
	// ExtraDefaults are built-in values components contribute (the
	// storage drivers' own settings), flat dotted keys. Defaults wins
	// on a key both set.
	ExtraDefaults map[string]any
}

// Paths fills the conventional file paths for ref.
func Paths(ref profile.Ref) (system, user, prof string, err error) {
	dir, err := xdg.RawConfigDir(Tool)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve config dir: %w", err)
	}
	opts := kitconfig.OptionsForTool(Tool)
	return opts.SystemConfigPath, filepath.Join(dir, "config.yaml"), ProfilePath(dir, ref), nil
}

// ProfilePath returns <configDir>/profiles/<pdir>.yaml.
func ProfilePath(configDir string, ref profile.Ref) string {
	return filepath.Join(configDir, "profiles", ref.Dir()+".yaml")
}

// Layer is one rung of the resolution chain.
type Layer struct {
	Origin Origin
	Path   string
	Exists bool
}

type fileLayer struct {
	Layer
	values map[string]any
}

// State is the loaded configuration of one invocation.
type State struct {
	in        Inputs
	v         *viper.Viper
	defaults  map[string]any
	files     []fileLayer // lowest precedence first
	overrides map[string]any
}

// Profile returns the selected profile.
func (s *State) Profile() profile.Ref { return s.in.Profile }

// ProfilePath returns the selected profile's config file.
func (s *State) ProfilePath() string { return s.in.ProfilePath }

// UserPath returns the user config file.
func (s *State) UserPath() string { return s.in.UserPath }

// Load layers heo's configuration for in.Profile into v, which must
// already be bound to the environment (kit config.BindEnv).
func Load(v *viper.Viper, in Inputs) (*State, error) {
	own, err := Defaults(in.Profile)
	if err != nil {
		return nil, err
	}
	defaults := make(map[string]any, len(own)+len(in.ExtraDefaults))
	for k, val := range in.ExtraDefaults {
		defaults[strings.ToLower(k)] = val
	}
	for k, val := range own {
		defaults[k] = val
	}
	for k, val := range defaults {
		v.SetDefault(k, val)
	}
	s := &State{in: in, v: v, defaults: defaults}

	conventional := []struct {
		origin Origin
		path   string
	}{
		{OriginSystem, in.SystemPath},
		{OriginUser, in.UserPath},
		{OriginProfile, in.ProfilePath},
	}
	for _, c := range conventional {
		l, err := readLayer(c.origin, c.path, false)
		if err != nil {
			return nil, err
		}
		if c.origin == OriginProfile && !l.Exists && in.RequireProfileFile &&
			!in.Profile.IsAps() && in.Profile != profile.Default() {
			e := output.NotFoundError(fmt.Sprintf("profile %s has no config file (%s)", in.Profile, c.path))
			e.SuggestedFix = fmt.Sprintf("create it with `heo --profile %s config set <key> <value>`, or check the name", in.Profile.ID)
			return nil, e
		}
		s.files = append(s.files, l)
	}
	for _, path := range in.Files {
		l, err := readLayer(OriginFile, path, true)
		if err != nil {
			return nil, err
		}
		s.files = append(s.files, l)
	}
	for _, l := range s.files {
		if !l.Exists {
			continue
		}
		if _, ok := l.values[KeyProfile]; ok {
			return nil, profileKeyError(fmt.Sprintf("%s sets %q", l.Path, KeyProfile))
		}
		if err := v.MergeConfigMap(unflatten(l.values)); err != nil {
			return nil, fmt.Errorf("config %s: %w", l.Path, err)
		}
	}

	s.overrides = map[string]any{}
	flatten("", in.Overrides, s.overrides)
	if _, ok := s.overrides[KeyProfile]; ok {
		return nil, profileKeyError("-c profile=... is not allowed")
	}
	// One Set per leaf, the same flat keys origin reporting uses.
	for k, val := range s.overrides {
		v.Set(k, val)
	}
	return s, nil
}

func profileKeyError(what string) *output.Error {
	e := output.UsageError(what + ": the profile is selected with --profile or HEO_PROFILE, never from configuration")
	e.SuggestedFix = "remove the profile key and pass --profile <ref> or set HEO_PROFILE"
	return e
}

// readLayer reads one YAML file into flat lower-cased keys, the shape
// viper reports. A missing optional file is an empty layer.
func readLayer(origin Origin, path string, required bool) (fileLayer, error) {
	l := fileLayer{Layer: Layer{Origin: origin, Path: path}, values: map[string]any{}}
	if path == "" {
		return l, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !required {
			return l, nil
		}
		return l, fmt.Errorf("config %s: %w", path, err)
	}
	if info.IsDir() {
		return l, fmt.Errorf("config %s: is a directory", path)
	}
	fv := viper.New()
	fv.SetConfigFile(path)
	fv.SetConfigType("yaml")
	if err := fv.ReadInConfig(); err != nil {
		return l, fmt.Errorf("config %s: %w", path, err)
	}
	for _, k := range fv.AllKeys() {
		l.values[k] = fv.Get(k)
	}
	l.Exists = true
	return l, nil
}

// flatten writes the leaves of a nested map as dotted lower-case keys.
func flatten(prefix string, in map[string]any, out map[string]any) {
	for k, val := range in {
		key := strings.ToLower(k)
		if prefix != "" {
			key = prefix + "." + key
		}
		if m, ok := val.(map[string]any); ok && len(m) > 0 {
			flatten(key, m, out)
			continue
		}
		out[key] = val
	}
}

// unflatten turns dotted keys back into a nested map for viper.
func unflatten(flat map[string]any) map[string]any {
	out := map[string]any{}
	for k, val := range flat {
		parts := strings.Split(k, ".")
		cur := out
		for _, p := range parts[:len(parts)-1] {
			next, ok := cur[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[p] = next
			}
			cur = next
		}
		cur[parts[len(parts)-1]] = val
	}
	return out
}

// Chain returns the resolution chain, highest precedence first: -c
// files (last first), profile, user, system, then built-in defaults.
func (s *State) Chain() []Layer {
	out := make([]Layer, 0, len(s.files)+1)
	for i := len(s.files) - 1; i >= 0; i-- {
		out = append(out, s.files[i].Layer)
	}
	return append(out, Layer{Origin: OriginDefault, Path: "<defaults>", Exists: true})
}

// Entry is one effective configuration value.
type Entry struct {
	Key    string `json:"key"    yaml:"key"    table:"KEY"`
	Value  string `json:"value"  yaml:"value"  table:"VALUE"`
	Origin Origin `json:"origin" yaml:"origin" table:"ORIGIN"`
	Detail string `json:"detail" yaml:"detail" table:"DETAIL"`
}

// Entries returns every effective value heo knows of, sorted by key,
// with its origin; credential material is redacted.
func (s *State) Entries() []Entry {
	keys := map[string]struct{}{}
	for k := range s.defaults {
		keys[k] = struct{}{}
	}
	for _, l := range s.files {
		for k := range l.values {
			keys[k] = struct{}{}
		}
	}
	for k := range s.overrides {
		keys[k] = struct{}{}
	}
	out := []Entry{s.profileEntry()}
	for k := range keys {
		origin, detail := s.origin(k)
		out = append(out, Entry{Key: k, Value: Display(k, s.v.Get(k)), Origin: origin, Detail: detail})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Lookup returns the entries for key: the one value when key is a leaf,
// every leaf below it when it names a section.
func (s *State) Lookup(key string) []Entry {
	key = strings.ToLower(key)
	var out []Entry
	for _, e := range s.Entries() {
		if e.Key == key || strings.HasPrefix(e.Key, key+".") {
			out = append(out, e)
		}
	}
	return out
}

func (s *State) profileEntry() Entry {
	origin, detail := s.in.ProfileOrigin, s.in.ProfileDetail
	if origin == "" {
		origin, detail = OriginDefault, "built-in"
	}
	return Entry{Key: KeyProfile, Value: s.in.Profile.String(), Origin: origin, Detail: detail}
}

// origin finds the highest layer that sets key, in viper's own order.
func (s *State) origin(key string) (Origin, string) {
	if _, ok := s.overrides[key]; ok {
		return OriginOverride, "-c " + key
	}
	if s.in.Flags != nil {
		if name, changed := s.in.Flags(key); changed {
			return OriginFlag, "--" + name
		}
	}
	if name := s.EnvName(key); name != "" {
		if val, ok := os.LookupEnv(name); ok && val != "" {
			return OriginEnv, name
		}
	}
	for i := len(s.files) - 1; i >= 0; i-- {
		if _, ok := s.files[i].values[key]; ok {
			return s.files[i].Origin, s.files[i].Path
		}
	}
	return OriginDefault, "built-in"
}

// EnvName returns the environment variable that sets key.
func (s *State) EnvName(key string) string {
	if s.in.EnvPrefix == "" {
		return ""
	}
	return strings.ToUpper(s.in.EnvPrefix + "_" + strings.ReplaceAll(key, ".", "_"))
}
