package storage

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/viper"
)

// Keys heo reads for storage itself; driver settings live under
// storage.<driver>.
const (
	// KeyDriver selects the driver.
	KeyDriver = "storage.driver"
	// KeyBackupDir is where backups are written.
	KeyBackupDir = "storage.backup.dir"
)

// nameRe bounds a driver name: it is a config key segment.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Registry holds the drivers a heo binary can run on. The composition
// root builds it by explicit registration; there is no global registry.
type Registry struct {
	drivers map[string]Driver
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{drivers: map[string]Driver{}}
}

// Register adds d. A name that is empty, malformed or already taken is
// an error.
func (r *Registry) Register(d Driver) error {
	if d == nil {
		return fmt.Errorf("storage: register nil driver")
	}
	name := d.Name()
	if !nameRe.MatchString(name) {
		return fmt.Errorf("storage: driver name %q: want lower-case letters, digits and '_'", name)
	}
	if _, dup := r.drivers[name]; dup {
		return fmt.Errorf("storage: driver %q registered twice", name)
	}
	r.drivers[name] = d
	return nil
}

// Names returns the registered driver names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.drivers))
	for n := range r.drivers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Lookup returns the driver called name, or an error naming every
// registered driver.
func (r *Registry) Lookup(name string) (Driver, error) {
	if d, ok := r.drivers[name]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("unknown storage driver %q; registered drivers: %s", name, r.namesText())
}

func (r *Registry) namesText() string {
	names := r.Names()
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// Defaults returns every registered driver's built-in settings as flat
// storage.<driver>.<key> keys.
func (r *Registry) Defaults() map[string]any {
	out := map[string]any{}
	for name, d := range r.drivers {
		for k, v := range d.Defaults() {
			out[driverKey(name, k)] = v
		}
	}
	return out
}

// Select returns the driver storage.driver names in v and its settings.
func (r *Registry) Select(v *viper.Viper) (Driver, Settings, error) {
	d, err := r.Lookup(v.GetString(KeyDriver))
	if err != nil {
		return nil, nil, err
	}
	return d, ViperSettings(v, d.Name()), nil
}

// Issue is one storage configuration finding, keyed by its full config
// key.
type Issue struct {
	Key     string
	Message string
	Warning bool
}

// Check validates the storage configuration in v offline: the selected
// driver must be registered, and its settings must pass its own check.
func (r *Registry) Check(v *viper.Viper) []Issue {
	d, s, err := r.Select(v)
	if err != nil {
		return []Issue{{Key: KeyDriver, Message: err.Error()}}
	}
	var out []Issue
	for _, p := range d.Validate(s) {
		out = append(out, Issue{Key: driverKey(d.Name(), p.Key), Message: p.Message, Warning: p.Warning})
	}
	return out
}

func driverKey(driver, key string) string { return "storage." + driver + "." + key }

// ViperSettings reads driver's settings from v.
func ViperSettings(v *viper.Viper, driver string) Settings {
	return viperSettings{v: v, prefix: "storage." + driver + "."}
}

type viperSettings struct {
	v      *viper.Viper
	prefix string
}

func (s viperSettings) GetString(key string) string { return s.v.GetString(s.prefix + key) }
func (s viperSettings) GetInt(key string) int       { return s.v.GetInt(s.prefix + key) }

// MapSettings is Settings over a map, for tests and callers that build
// settings by hand.
type MapSettings map[string]any

// GetString returns key's value as a string.
func (m MapSettings) GetString(key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// GetInt returns key's value as an int, 0 when unset or not a number.
func (m MapSettings) GetInt(key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}
