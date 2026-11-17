package assist

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/viper"
)

// Problem is one finding of Check.
type Problem struct {
	Key     string
	Warning bool
	Message string
}

// known are the keys of the llm section below the cloud opt-ins.
var known = []string{KeyProvider, KeyModel, KeyEndpoint, KeyTimeout, KeyMaxPromptChars}

// Check validates the llm section offline: nothing is contacted and no
// credential is read. The configuration must resolve to one provider,
// every key must be one heo reads, and an opt-in that names a provider
// the configuration does not select is flagged.
func Check(v *viper.Viper) []Problem {
	var out []Problem
	c := ConfigFrom(v)
	if _, err := c.Plan(); err != nil {
		key := KeyProvider
		var pe *planError
		if errors.As(err, &pe) {
			key = pe.Key
		}
		msg := err.Error()
		if pe != nil && pe.Err.SuggestedFix != "" {
			msg += "; " + pe.Err.SuggestedFix
		}
		out = append(out, Problem{Key: key, Message: msg})
	}
	selected := c.Provider
	if selected == "" {
		selected = ProviderOllama
	}
	for _, k := range slices.Sorted(slices.Values(v.AllKeys())) {
		if !strings.HasPrefix(k, "llm.") || slices.Contains(known, k) {
			continue
		}
		name, field, ok := cloudField(k)
		switch {
		case !ok:
			out = append(out, Problem{Key: k, Message: fmt.Sprintf("unknown key; known: %s, %s.<provider>.enabled, %s.<provider>.credential_ref, %s.<provider>.type, %s.<provider>.endpoint",
				strings.Join(known, ", "), strings.TrimSuffix(cloudPrefix, "."), strings.TrimSuffix(cloudPrefix, "."),
				strings.TrimSuffix(cloudPrefix, "."), strings.TrimSuffix(cloudPrefix, "."))})
		case field == "enabled" && v.GetBool(k) && name != selected:
			out = append(out, Problem{Key: k, Warning: true, Message: fmt.Sprintf(
				"opts in to %s, but %s is %q: the opt-in has no effect until %s names it", name, KeyProvider, selected, KeyProvider)})
		}
	}
	return out
}

// cloudField splits llm.cloud.<name>.<field>; ok is false for any other
// key, and for a field heo does not read.
func cloudField(key string) (name, field string, ok bool) {
	rest, found := strings.CutPrefix(key, cloudPrefix)
	if !found {
		return "", "", false
	}
	name, field, found = strings.Cut(rest, ".")
	if !found || name == "" || (field != "enabled" && field != "credential_ref" && field != "type" && field != "endpoint") {
		return "", "", false
	}
	return name, field, true
}
