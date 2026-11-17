// Package hookstest is a fixture plugin for tests: a POSIX shell script
// that records the event IDs it was given and answers as its mode says.
package hookstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Modes of the fixture, its second argument; its first is the log file.
const (
	// ModeOK answers with one operation.
	ModeOK = "ok"
	// ModeEmpty answers with no output.
	ModeEmpty = "empty"
	// ModeExit1 and ModeExit2 exit with that code.
	ModeExit1 = "exit1"
	ModeExit2 = "exit2"
	// ModeExit3 exits 3, which is neither retryable nor permanent by the
	// plugin's own word: a crash.
	ModeExit3 = "exit3"
	// ModeKill kills itself with SIGKILL.
	ModeKill = "kill"
	// ModeFlaky exits 1 the first time it sees an event, then answers.
	ModeFlaky = "flaky"
	// ModeSlow never answers within a test.
	ModeSlow = "slow"
	// ModeBadJSON prints what is not a result.
	ModeBadJSON = "badjson"
	// ModeEnv records its environment to <log>.env, then answers.
	ModeEnv = "env"
	// ModeBig prints a 5000-byte line, more than a small output cap.
	ModeBig = "big"
	// ModeStderr writes two lines to stderr, the second unterminated,
	// then answers.
	ModeStderr = "stderr"
)

const script = `#!/bin/sh
# fixture plugin: <log> <mode>; exec mode reads one envelope, stream mode
# (mode suffix ":stream") answers one line per envelope.
log=$1
mode=$2
case "$mode" in
*:stream) mode=${mode%:stream}; stream=1 ;;
esac
answer() {
	id=$1
	case "$mode" in
	ok)      printf '{"event_id":"%s","operations":[{"verb":"message note","args":["%s"]}]}\n' "$id" "$id" ;;
	empty)   if [ -n "$stream" ]; then printf '{"event_id":"%s","operations":[]}\n' "$id"; fi ;;
	badjson) printf 'not json\n' ;;
	slow)    sleep 30 ;;
	exit1)   exit 1 ;;
	exit2)   exit 2 ;;
	exit3)   exit 3 ;;
	kill)    kill -9 $$ ;;
	env)     env > "$log.env"; pwd > "$log.pwd"; printf '{"event_id":"%s","operations":[]}\n' "$id" ;;
	big)     head -c 5000 /dev/zero | tr '\0' x; printf '\n' ;;
	stderr)  printf 'first line\nsecond' >&2; printf '{"event_id":"%s","operations":[]}\n' "$id" ;;
	flaky)
		if [ ! -e "$log.flaky.$id" ]; then : > "$log.flaky.$id"; exit 1; fi
		printf '{"event_id":"%s","operations":[]}\n' "$id" ;;
	esac
}
idof() { printf '%s' "$1" | sed -n 's/.*"event_id":"\([^"]*\)".*/\1/p'; }
if [ -n "$stream" ]; then
	while IFS= read -r line; do
		id=$(idof "$line")
		printf '%s\n' "$id" >> "$log"
		answer "$id"
	done
else
	line=$(cat)
	id=$(idof "$line")
	printf '%s\n' "$id" >> "$log"
	answer "$id"
fi
`

// Plugin writes the fixture into a temporary directory and returns its
// path and the log file it appends event IDs to.
func Plugin(t testing.TB) (command, log string) {
	t.Helper()
	dir := t.TempDir()
	command = filepath.Join(dir, "plugin.sh")
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil { // #nosec G306 -- an executable test fixture
		t.Fatal(err)
	}
	return command, filepath.Join(dir, "events.log")
}

// Seen returns the event IDs the fixture logged, in order.
func Seen(log string) []string {
	b, err := os.ReadFile(filepath.Clean(log))
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}
