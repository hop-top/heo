// Package plugins reads and validates the plugins: configuration
// section: the hook plugins the runner starts, what events each sees
// and how it is run, bounded and retried.
//
// A plugin acts as the actor plugin:<name> and holds only the roles
// authorization.grants gives that actor; nothing here grants a role.
package plugins

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/credentials"
	"hop.top/uma/model"
)

// KeyPlugins is the configuration key of the plugin list.
const KeyPlugins = "plugins"

// Process modes.
const (
	// ModeExec starts a fresh process per event: one envelope in, one
	// result out. The default.
	ModeExec = "exec"
	// ModeStream keeps one process and feeds it newline-delimited
	// envelopes, reading newline-delimited results.
	ModeStream = "stream"
)

// Defaults for the fields a plugin leaves out.
const (
	DefaultMode        = ModeExec
	DefaultTimeout     = 30 * time.Second
	DefaultConcurrency = 1
	DefaultMaxAttempts = 5
	DefaultBackoffInit = time.Second
	DefaultBackoffMax  = 5 * time.Minute

	// MaxConcurrency and MaxAttempts bound what a plugin may ask for.
	MaxConcurrency = 64
	MaxAttempts    = 100
)

// Filter selects the events a plugin receives. Every list is an
// allow-list; an empty one admits all. Kinds must be in the hooks
// event catalog.
type Filter struct {
	Kinds    []string
	Channels []string
	Inboxes  []string
}

// Backoff is the wait between attempts: Initial before the second,
// doubling up to Max.
type Backoff struct {
	Initial time.Duration
	Max     time.Duration
}

// Retry bounds redelivery of one event: MaxAttempts counts the first
// attempt; after the last the event is dead-lettered.
type Retry struct {
	MaxAttempts int
	Backoff     Backoff
}

// Plugin is one configured hook plugin, defaults applied.
type Plugin struct {
	Name        string
	Command     string   // executable; exclusive with Socket
	Args        []string // arguments to Command
	Socket      string   // unix socket path of a running plugin; exclusive with Command
	Mode        string
	Filter      Filter
	Timeout     time.Duration
	Concurrency int
	Retry       Retry
	// DryRun records what the plugin's results would apply and applies
	// nothing. Off by default.
	DryRun bool
}

// Actor is the actor the plugin runs as.
func (p Plugin) Actor() model.ActorID { return credentials.PluginActor(p.Name) }

// Problem is one finding about the plugins configuration.
type Problem struct {
	Key     string
	Message string
	Warning bool
}

// EventKinds are the builtin uma event kinds a plugin may subscribe to,
// the catalog in docs/manual/hooks.md. A test fails when uma gains a
// kind this list lacks.
var EventKinds = []string{
	model.EventMessageReceived,
	model.EventMessageCreated,
	model.EventMessageUpdated,
	model.EventConversationChanged,
	model.EventConversationLabelAdded,
	model.EventConversationLabelRemoved,
	model.EventConversationAssigned,
	model.EventConversationUnassigned,
	model.EventConversationSnoozed,
	model.EventConversationUnsnoozed,
	model.EventConversationLinked,
	model.EventConversationUnlinked,
	model.EventDeliveryUpdated,
	model.EventInteractionReceived,
	model.EventInteractionUpdated,
	model.EventWorkItemUpdated,
	model.EventTaskUpdated,
	model.EventOperationHeld,
	model.EventOperationDecided,
	model.EventOperationExpired,
	model.EventIdentityLinked,
	model.EventContactCreated,
	model.EventContactUpdated,
	model.EventContactMerged,
}

var (
	pluginFields  = []string{"name", "command", "args", "socket", "mode", "filter", "timeout", "concurrency", "retry", "dry_run"}
	filterFields  = []string{"kinds", "channels", "inboxes"}
	retryFields   = []string{"max_attempts", "backoff"}
	backoffFields = []string{"initial", "max"}
)

// Read parses the plugins section structurally: shapes, names, modes,
// kinds, bounds and duplicate names. It looks at nothing outside the
// configuration; Check adds grants and executables.
func Read(v *viper.Viper) ([]Plugin, []Problem) {
	raw := v.Get(KeyPlugins)
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, []Problem{{Key: KeyPlugins, Message: "want a list of plugin entries"}}
	}
	var out []Plugin
	var probs []Problem
	seen := map[string]int{}
	for i, e := range list {
		key := fmt.Sprintf("%s[%d]", KeyPlugins, i)
		m, ok := e.(map[string]any)
		if !ok {
			probs = append(probs, Problem{Key: key, Message: "want an entry with a name and a command or socket"})
			continue
		}
		p, pp := readPlugin(key, m)
		probs = append(probs, pp...)
		if p.Name != "" {
			if first, dup := seen[p.Name]; dup {
				probs = append(probs, Problem{Key: key + ".name", Message: fmt.Sprintf(
					"%q already named by %s[%d]: plugin names are unique", p.Name, KeyPlugins, first)})
			} else {
				seen[p.Name] = i
			}
		}
		out = append(out, p)
	}
	return out, probs
}

func readPlugin(key string, m map[string]any) (Plugin, []Problem) {
	var probs []Problem
	bad := func(k, msg string) { probs = append(probs, Problem{Key: key + "." + k, Message: msg}) }
	unknown(key, m, pluginFields, &probs)

	p := Plugin{Mode: DefaultMode, Timeout: DefaultTimeout, Concurrency: DefaultConcurrency,
		Retry: Retry{MaxAttempts: DefaultMaxAttempts, Backoff: Backoff{Initial: DefaultBackoffInit, Max: DefaultBackoffMax}}}

	if s, ok := str(m["name"]); !ok || s == "" {
		bad("name", "required: the plugin's name, which makes its actor plugin:<name>")
	} else if err := credentials.ValidPluginName(s); err != nil {
		bad("name", err.Error())
	} else {
		p.Name = s
	}

	if r, set := m["command"]; set {
		if s, ok := str(r); ok && s != "" {
			p.Command = s
		} else {
			bad("command", "want an executable path or name")
		}
	}
	if r, set := m["socket"]; set {
		if s, ok := str(r); ok && filepath.IsAbs(s) {
			p.Socket = s
		} else {
			bad("socket", "want an absolute unix socket path")
		}
	}
	switch hasCmd, hasSock := m["command"] != nil, m["socket"] != nil; {
	case hasCmd && hasSock:
		bad("socket", "set command or socket, not both")
	case !hasCmd && !hasSock:
		bad("command", "required: set command or socket")
	}
	if r, set := m["args"]; set {
		if p.Command == "" {
			bad("args", "only with command")
		}
		if args, ok := strList(r); ok {
			p.Args = args
		} else {
			bad("args", "want a list of strings")
		}
	}

	if r, set := m["mode"]; set {
		s, _ := str(r)
		if s != ModeExec && s != ModeStream {
			bad("mode", fmt.Sprintf("%v: want %s or %s", r, ModeExec, ModeStream))
		} else {
			p.Mode = s
		}
	}

	if r, set := m["filter"]; set {
		p.Filter = readFilter(key+".filter", r, &probs)
	}

	if r, set := m["timeout"]; set {
		if d, ok := duration(r); ok && d > 0 {
			p.Timeout = d
		} else {
			bad("timeout", fmt.Sprintf("%v: want a positive duration such as 30s", r))
		}
	}
	if r, set := m["concurrency"]; set {
		if n, ok := integer(r); ok && n >= 1 && n <= MaxConcurrency {
			p.Concurrency = n
		} else {
			bad("concurrency", fmt.Sprintf("%v: want an integer from 1 to %d", r, MaxConcurrency))
		}
	}
	if p.Socket != "" && p.Concurrency > 1 {
		bad("concurrency", "socket is one ordered connection: concurrency is 1")
	}
	if p.Mode == ModeStream && p.Concurrency > 1 {
		bad("concurrency", "stream mode is one ordered process: concurrency is 1")
	}

	if r, set := m["dry_run"]; set {
		if b, ok := r.(bool); ok {
			p.DryRun = b
		} else {
			bad("dry_run", fmt.Sprintf("%v: want true or false", r))
		}
	}

	if r, set := m["retry"]; set {
		p.Retry = readRetry(key+".retry", r, p.Retry, &probs)
	}
	return p, probs
}

func readFilter(key string, raw any, probs *[]Problem) Filter {
	var f Filter
	m, ok := raw.(map[string]any)
	if !ok {
		*probs = append(*probs, Problem{Key: key, Message: "want kinds, channels and inboxes lists"})
		return f
	}
	unknown(key, m, filterFields, probs)
	fields := []struct {
		name string
		dst  *[]string
	}{{"kinds", &f.Kinds}, {"channels", &f.Channels}, {"inboxes", &f.Inboxes}}
	for _, fl := range fields {
		r, set := m[fl.name]
		if !set {
			continue
		}
		list, ok := strList(r)
		if !ok {
			*probs = append(*probs, Problem{Key: key + "." + fl.name, Message: "want a list of strings"})
			continue
		}
		for j, s := range list {
			ek := fmt.Sprintf("%s.%s[%d]", key, fl.name, j)
			switch {
			case strings.TrimSpace(s) == "":
				*probs = append(*probs, Problem{Key: ek, Message: "empty"})
			case fl.name == "kinds" && !slices.Contains(EventKinds, s):
				*probs = append(*probs, Problem{Key: ek, Message: fmt.Sprintf(
					"unknown event kind %q: a plugin may subscribe to the kinds in the hooks event catalog", s)})
			}
		}
		*fl.dst = list
	}
	return f
}

func readRetry(key string, raw any, def Retry, probs *[]Problem) Retry {
	r := def
	m, ok := raw.(map[string]any)
	if !ok {
		*probs = append(*probs, Problem{Key: key, Message: "want max_attempts and backoff"})
		return r
	}
	unknown(key, m, retryFields, probs)
	if x, set := m["max_attempts"]; set {
		if n, ok := integer(x); ok && n >= 1 && n <= MaxAttempts {
			r.MaxAttempts = n
		} else {
			*probs = append(*probs, Problem{Key: key + ".max_attempts", Message: fmt.Sprintf(
				"%v: want an integer from 1 to %d (1 means no retry)", x, MaxAttempts)})
		}
	}
	if x, set := m["backoff"]; set {
		bm, ok := x.(map[string]any)
		if !ok {
			*probs = append(*probs, Problem{Key: key + ".backoff", Message: "want initial and max durations"})
			return r
		}
		unknown(key+".backoff", bm, backoffFields, probs)
		for _, f := range []struct {
			name string
			dst  *time.Duration
		}{{"initial", &r.Backoff.Initial}, {"max", &r.Backoff.Max}} {
			x, set := bm[f.name]
			if !set {
				continue
			}
			if d, ok := duration(x); ok && d > 0 {
				*f.dst = d
			} else {
				*probs = append(*probs, Problem{Key: key + ".backoff." + f.name, Message: fmt.Sprintf(
					"%v: want a positive duration such as 5m", x)})
			}
		}
		if r.Backoff.Max < r.Backoff.Initial {
			*probs = append(*probs, Problem{Key: key + ".backoff.max", Message: fmt.Sprintf(
				"%s is below initial %s", r.Backoff.Max, r.Backoff.Initial)})
		}
	}
	return r
}

// Check validates the plugins offline, for `config validate` and
// `service start`: Read's findings plus, for each plugin, a grant for
// plugin:<name> in authorization.grants and a command that resolves to
// an executable. A socket is not probed; the plugin may start later.
func Check(v *viper.Viper) []Problem {
	list, probs := Read(v)
	for i, p := range list {
		key := fmt.Sprintf("%s[%d]", KeyPlugins, i)
		if p.Name != "" && !authz.HasGrant(v, p.Actor()) {
			probs = append(probs, Problem{Key: key + ".name", Message: fmt.Sprintf(
				"no authorization.grants entry for %s: a plugin holds only the roles granted to its actor", p.Actor())})
		}
		if p.Command != "" {
			if _, err := exec.LookPath(p.Command); err != nil {
				probs = append(probs, Problem{Key: key + ".command", Message: fmt.Sprintf("command not found: %v", err)})
			}
		}
	}
	return probs
}

func unknown(key string, m map[string]any, fields []string, probs *[]Problem) {
	var extra []string
	for k := range m {
		if !slices.Contains(fields, k) {
			extra = append(extra, k)
		}
	}
	slices.Sort(extra)
	for _, k := range extra {
		*probs = append(*probs, Problem{Key: key + "." + k, Message: "unknown field; fields: " + strings.Join(fields, ", ")})
	}
}

func str(r any) (string, bool) { s, ok := r.(string); return s, ok }

func strList(r any) ([]string, bool) {
	l, ok := r.([]any)
	if !ok {
		if sl, ok := r.([]string); ok {
			return sl, true
		}
		return nil, false
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func integer(r any) (int, bool) {
	switch n := r.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	}
	return 0, false
}

func duration(r any) (time.Duration, bool) {
	switch d := r.(type) {
	case string:
		x, err := time.ParseDuration(d)
		return x, err == nil
	case time.Duration:
		return d, true
	}
	return 0, false
}
