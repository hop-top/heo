package xrrx

import (
	"errors"
	"fmt"
	"os"
	"strings"

	xrr "hop.top/xrr"
)

// Environment a child process reads, beyond xrr's own XRR_MODE and
// XRR_CASSETTE_DIR.
const (
	// EnvMissLog names a file a replay miss is appended to, so the
	// parent test can fail on a miss the child survived. Optional.
	EnvMissLog = "HEO_E2E_XRR_MISS_LOG"

	envCI = "CI"
)

// Env is the cassette configuration of one process.
type Env struct {
	// Mode is the session mode.
	Mode xrr.Mode
	// Dir is the process's own cassette directory. Passthrough has none.
	Dir string
	// MissLog is the file replay misses are appended to; empty for none.
	MissLog string
	// GitHub is the sandbox a live recording may reach: set only in
	// record mode with the live flag (CheckLive). Without it
	// api.github.com is unreachable through this process's transport.
	GitHub *GitHubSandbox
	// Twilio is the account a live recording may reach, set in the same
	// way (CheckLiveTwilio).
	Twilio *TwilioSandbox
	// Slack is the workspace and channel a live recording may reach, set
	// in the same way (CheckLiveSlack).
	Slack *SlackSandbox
	// Telegram is the bot and chat a live recording may reach, set in the
	// same way (CheckLiveTelegram).
	Telegram *TelegramSandbox
	// Discord is the channel of a Discord server a live recording may
	// reach, set in the same way (CheckLiveDiscord).
	Discord *DiscordSandbox
	// Teams is the bot and personal chat a live recording may reach, set
	// in the same way (CheckLiveTeams).
	Teams *TeamsSandbox
}

// ErrRecordInCI is returned for XRR_MODE=record while CI is set.
var ErrRecordInCI = errors.New("xrrx: recording is refused when CI is set; record locally and commit the cassettes")

// CheckRecord reports whether this environment may record: not in CI,
// and with xrr's redaction left on. getenv is os.Getenv in a process.
func CheckRecord(getenv func(string) string) error {
	if ci := strings.ToLower(strings.TrimSpace(getenv(envCI))); ci != "" && ci != "0" && ci != "false" {
		return ErrRecordInCI
	}
	for _, k := range []string{xrr.EnvRedactDisable, xrr.EnvRedactAllow} {
		if getenv(k) != "" {
			return fmt.Errorf("xrrx: recording is refused with %s set: it lets credentials into a cassette", k)
		}
	}
	return nil
}

// ParseMode validates a mode name; empty is replay, the default of every
// test run.
func ParseMode(s string) (xrr.Mode, error) {
	switch m := xrr.Mode(s); m {
	case "":
		return xrr.ModeReplay, nil
	case xrr.ModeRecord, xrr.ModeReplay, xrr.ModePassthrough:
		return m, nil
	}
	return "", fmt.Errorf("xrrx: %s=%q is not a valid mode (want record, replay or passthrough)", xrr.EnvMode, s)
}

// FromEnv reads the process's cassette configuration: nil, nil when
// XRR_MODE is unset (the process runs unrecorded), an error for a
// configuration xrr refuses or a record this environment may not make.
func FromEnv() (*Env, error) {
	// Record is judged before xrr creates its directory.
	if xrr.Mode(os.Getenv(xrr.EnvMode)) == xrr.ModeRecord {
		if err := CheckRecord(os.Getenv); err != nil {
			return nil, err
		}
	}
	// xrr validates the variables and creates (record) or requires
	// (replay) the directory.
	sess, err := xrr.SessionFromEnv()
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil
	}
	e := &Env{Mode: sess.Mode(), MissLog: os.Getenv(EnvMissLog)}
	if e.Mode == xrr.ModeRecord && os.Getenv(EnvLive) != "" {
		if os.Getenv(EnvDiscordToken) != "" {
			if err := CheckLiveDiscord(os.Getenv); err != nil {
				return nil, err
			}
			dc, err := DiscordSandboxFromEnv(os.Getenv)
			if err != nil {
				return nil, err
			}
			e.Discord = dc
		} else if os.Getenv(EnvTeamsConnectorToken) != "" {
			if err := CheckLiveTeams(os.Getenv); err != nil {
				return nil, err
			}
			tm, err := TeamsSandboxFromEnv(os.Getenv)
			if err != nil {
				return nil, err
			}
			e.Teams = tm
		} else if os.Getenv(EnvTelegramToken) != "" {
			if err := CheckLiveTelegram(os.Getenv); err != nil {
				return nil, err
			}
			tg, err := TelegramSandboxFromEnv(os.Getenv)
			if err != nil {
				return nil, err
			}
			e.Telegram = tg
		} else if os.Getenv(EnvSlackToken) != "" {
			if err := CheckLiveSlack(os.Getenv); err != nil {
				return nil, err
			}
			sl, err := SlackSandboxFromEnv(os.Getenv)
			if err != nil {
				return nil, err
			}
			e.Slack = sl
		} else if os.Getenv(EnvTwilioAccountSID) != "" && os.Getenv(EnvGitHubRepo) == "" {
			if err := CheckLiveTwilio(os.Getenv); err != nil {
				return nil, err
			}
			t, err := TwilioSandboxFromEnv(os.Getenv)
			if err != nil {
				return nil, err
			}
			e.Twilio = t
		} else {
			if err := CheckLive(os.Getenv); err != nil {
				return nil, err
			}
			g, err := GitHubSandboxFromEnv(os.Getenv)
			if err != nil {
				return nil, err
			}
			e.GitHub = g
		}
	}
	if e.Mode != xrr.ModePassthrough {
		e.Dir = os.Getenv(xrr.EnvCassetteDir)
	}
	return e, nil
}

// session is the xrr session for the exec adapter. HTTP builds its own
// through xrrtest on the same directory.
func (e *Env) session() (*xrr.FileSession, error) {
	if e.Mode == xrr.ModePassthrough {
		return xrr.NewSession(e.Mode, nil), nil
	}
	if e.Mode == xrr.ModeRecord {
		// xrr's file cassette does not create its directory.
		if err := os.MkdirAll(e.Dir, 0o750); err != nil {
			return nil, fmt.Errorf("xrrx: %w", err)
		}
	}
	return xrr.NewSession(e.Mode, xrr.NewFileCassette(e.Dir)), nil
}

// MustFromEnv is FromEnv for a binary's wiring, where a bad
// configuration has nowhere to be returned to: it says why on stderr and
// exits 2.
func MustFromEnv() *Env {
	e, err := FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	return e
}
