package xrrx

import (
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Placeholders the scrub rules leave in a cassette.
const (
	// LoopbackHost replaces a loopback host:port: the port is the OS's
	// to allocate, so it can neither be recorded nor matched.
	LoopbackHost = "loopback.invalid"
	// TempDir replaces a path under a temp directory.
	TempDir = "<tmp>"
	// Time replaces a timestamp in a request.
	Time = "<time>"
	// PID replaces a process id in a request.
	PID = "pid=<pid>"
)

var (
	// loopbackRe is a loopback address with a port. A bare host names
	// nothing the OS chose, so it stays.
	loopbackRe = regexp.MustCompile(`(?i)(?:127\.\d{1,3}\.\d{1,3}\.\d{1,3}|localhost|\[::1\]):\d{2,5}`)
	// tempRe is a path under a temp directory of the platforms heo runs
	// its tests on. The path ends at a character no path of ours holds.
	tempRe = regexp.MustCompile(`(?:/private)?(?:/var/folders/[^\s"'\\<>]+|/tmp/[^\s"'\\<>]+)`)
	// timeRe is an RFC 3339 timestamp.
	timeRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
	// markerRe is in github.go: the echo marker of a comment uma posts.
	// pidRe is a process id spelt "pid=123", "pid: 123" or "pid 123".
	pidRe = regexp.MustCompile(`(?i)\bpid[=: ]\s*\d+`)
)

// ScrubText removes from s what the OS or the test run chose and a
// replay cannot reproduce: loopback host:port pairs, temp paths and the
// operation and attempt of an echo marker. It is
// applied to requests and responses alike. The temp directory of this
// process is replaced wherever it appears, whatever its spelling.
func ScrubText(s string) string {
	if d := strings.TrimRight(os.TempDir(), "/"); len(d) > 1 {
		s = strings.ReplaceAll(s, d, TempDir)
	}
	s = tempRe.ReplaceAllString(s, TempDir)
	s = markerRe.ReplaceAllString(s, Marker)
	return loopbackRe.ReplaceAllString(s, LoopbackHost)
}

// ScrubRequestText is ScrubText plus what only a request must lose: a
// timestamp or process id in a request changes its fingerprint on every
// run. Responses keep theirs, as the adapter under test parses them.
func ScrubRequestText(s string) string {
	s = ScrubText(s)
	s = timeRe.ReplaceAllString(s, Time)
	return pidRe.ReplaceAllString(s, PID)
}

// scrubURL rewrites a URL's host:port and query the way ScrubRequestText
// does, keeping it a URL.
func scrubURL(u *url.URL) {
	if loopbackRe.MatchString(u.Host) {
		u.Host = LoopbackHost
	}
	if u.RawQuery != "" {
		// Not re-encoded: the scrub only swaps the placeholder in.
		u.RawQuery = ScrubRequestText(u.RawQuery)
	}
	u.Path = ScrubRequestText(u.Path)
	u.RawPath = ""
}
