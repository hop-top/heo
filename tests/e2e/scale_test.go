//go:build heoscale

package e2e

// Scale harness: one profile, one daemon, one channel per mailbox, run
// against local Gmail, Graph and token-endpoint stand-ins. It is not
// part of the suite: it needs the heoscale build tag and HEO_SCALE=1.
//
//	HEO_SCALE=1 HEO_REQUIRE_DOCKER=1 go test -tags heoscale -count=1 -timeout 60m \
//	    -run TestScaleMailChannels ./tests/e2e/
//
// Knobs (all optional):
//
//	HEO_SCALE_SIZES   channels per mail type, comma separated (default 5,50,100)
//	HEO_SCALE_PHASES  default, accel, broken (default all three)
//	HEO_SCALE_WINDOW  steady-state window after every channel synced (default 60s)
//	HEO_SCALE_SPREAD  services.receivers.start_spread for the run (default: the daemon's; 0s is no stagger; the broken phase defaults to 0s)
//	HEO_SCALE_OUT     directory the per-run JSON and status dumps go to
//
// Phases: "broken" stores no credential for the first tenth of each
// type's mailboxes and checks the degraded reporting: config validate
// with the daemon up exits 2 with an error finding for each channel
// whose configured-in credential is missing (the push microsoft ones),
// and status reads degraded with exactly those receivers
// failed. "default" leaves every renewal and poll setting at the
// adapter's default. "accel" shortens the clock the receivers see by
// having the stand-ins grant short lifetimes (a Gmail watch that expires
// in an hour plus watchTTL, a Graph subscription that expires in 15
// minutes plus watchTTL, an access token that lives a minute (kit
// refreshes a token at half its lifetime when that is under its 5 minute skew)) and by polling every pollMS. heo has no
// clock seam of its own; the adapters read time.Now through the host.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"hop.top/heo/internal/composition"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
	"hop.top/uma/model"
)

const (
	scaleEnv = "HEO_SCALE"

	accelWatchTTL = 10 * time.Second
	accelTokenTTL = 60 * time.Second
	accelPollMS   = 10_000
)

// scaleFake stands in for Gmail, Graph and their token endpoint. It
// counts every request, with its time, and the requests in flight.
type scaleFake struct {
	srv *httptest.Server

	mu          sync.Mutex
	t0          time.Time
	accel       bool
	events      []scaleEvent
	counts      map[string]int
	tokenBy     map[string]int
	firstHit    map[string]time.Duration
	inflight    int
	maxInflight int
	subSeq      int
	tokenSeq    int
}

type scaleEvent struct {
	at   time.Duration
	kind string
}

func newScaleFake(accel bool) *scaleFake {
	f := &scaleFake{accel: accel, t0: time.Now(), counts: map[string]int{}, tokenBy: map[string]int{}, firstHit: map[string]time.Duration{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *scaleFake) close() { f.srv.Close() }

func (f *scaleFake) gmailEndpoint() string { return f.srv.URL + "/gmail/v1" }
func (f *scaleFake) graphEndpoint() string { return f.srv.URL + "/v1.0" }
func (f *scaleFake) tokenURL() string      { return f.srv.URL + "/token" }

// mark notes a request of kind for mailbox (empty for none).
func (f *scaleFake) mark(kind, mailbox string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at := time.Since(f.t0)
	f.events = append(f.events, scaleEvent{at, kind})
	f.counts[kind]++
	if mailbox != "" {
		if _, ok := f.firstHit[mailbox]; !ok {
			f.firstHit[mailbox] = at
		}
	}
}

func (f *scaleFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.inflight++
	f.maxInflight = max(f.maxInflight, f.inflight)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	switch {
	case p == "/token":
		_ = r.ParseForm()
		id := r.PostForm.Get("client_id")
		f.mu.Lock()
		f.tokenBy[id]++
		f.tokenSeq++
		n := f.tokenSeq
		f.mu.Unlock()
		f.mark("token", "")
		exp := 3600
		if f.accel {
			exp = int(accelTokenTTL / time.Second)
		}
		fmt.Fprintf(w, `{"access_token":"at-%d","token_type":"Bearer","expires_in":%d}`, n, exp)
	case strings.HasPrefix(p, "/gmail/v1/users/"):
		seg := strings.Split(strings.TrimPrefix(p, "/gmail/v1/users/"), "/")
		user, op := seg[0], strings.Join(seg[1:], "/")
		switch op {
		case "profile":
			f.mark("gmail.profile", user)
			_, _ = io.WriteString(w, `{"emailAddress":"`+user+`","historyId":"1000"}`)
		case "messages":
			f.mark("gmail.messages", user)
			_, _ = io.WriteString(w, `{"messages":[]}`)
		case "history":
			f.mark("gmail.history", user)
			_, _ = io.WriteString(w, `{"history":[],"historyId":"1000"}`)
		case "watch":
			f.mark("gmail.watch", user)
			ttl := 7 * 24 * time.Hour
			if f.accel {
				ttl = time.Hour + accelWatchTTL
			}
			fmt.Fprintf(w, `{"historyId":"1000","expiration":"%d"}`, time.Now().Add(ttl).UnixMilli())
		default:
			f.mark("gmail.other", user)
			http.NotFound(w, r)
		}
	case strings.HasPrefix(p, "/v1.0/subscriptions"):
		f.subscriptions(w, r)
	case strings.HasPrefix(p, "/v1.0/users/"):
		seg := strings.Split(strings.TrimPrefix(p, "/v1.0/users/"), "/")
		mb := seg[0]
		switch {
		case len(seg) == 3 && seg[1] == "mailFolders":
			f.mark("graph.folder", mb)
			fmt.Fprintf(w, `{"id":%q,"displayName":%q}`, seg[2], seg[2])
		case len(seg) == 5 && seg[1] == "mailFolders" && seg[3] == "messages" && seg[4] == "delta":
			f.mark("graph.delta", mb)
			fmt.Fprintf(w, `{"value":[],"@odata.deltaLink":%q}`, f.srv.URL+p+"?$deltatoken=t")
		default:
			f.mark("graph.other", mb)
			http.NotFound(w, r)
		}
	default:
		f.mark("other", "")
		http.NotFound(w, r)
	}
}

func (f *scaleFake) subscriptions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Expiration time.Time `json:"expirationDateTime"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	exp := req.Expiration
	if f.accel {
		exp = time.Now().Add(15*time.Minute + accelWatchTTL)
	}
	switch r.Method {
	case http.MethodPost:
		f.mu.Lock()
		f.subSeq++
		n := f.subSeq
		f.mu.Unlock()
		f.mark("graph.sub.create", "")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":"sub-%d","expirationDateTime":%q}`, n, exp.UTC().Format(time.RFC3339))
	case http.MethodPatch:
		f.mark("graph.sub.renew", "")
		fmt.Fprintf(w, `{"id":%q,"expirationDateTime":%q}`, strings.TrimPrefix(r.URL.Path, "/v1.0/subscriptions/"), exp.UTC().Format(time.RFC3339))
	case http.MethodDelete:
		f.mark("graph.sub.delete", "")
		w.WriteHeader(http.StatusNoContent)
	default:
		f.mark("graph.sub.list", "")
		_, _ = io.WriteString(w, `{"value":[]}`)
	}
}

func (f *scaleFake) snapshot() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for k, v := range f.counts {
		out[k] = v
	}
	return out
}

func (f *scaleFake) idle() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		return 0
	}
	return time.Since(f.t0) - f.events[len(f.events)-1].at
}

func (f *scaleFake) lastEvent() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		return 0
	}
	return f.events[len(f.events)-1].at
}

func (f *scaleFake) synced(n int) (int, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var last time.Duration
	for _, d := range f.firstHit {
		last = max(last, d)
	}
	return len(f.firstHit), last
}

// scaleDaemon is a built daemon with its log and ready times.
type scaleDaemon struct {
	cmd   *exec.Cmd
	t0    time.Time
	done  chan struct{}
	exit  int
	mu    sync.Mutex
	log   strings.Builder
	ready map[string]time.Duration
	lines int
}

var scaleReadyRe = regexp.MustCompile(`ready_reported object=service .*service=([\w-]+)`)

func (s *sandbox) startScale(bin string, env []string, args ...string) *scaleDaemon {
	s.t.Helper()
	cmd := exec.Command(bin, args...) //nolint:gosec // the harness's own build
	cmd.Dir = s.path("cwd")
	for k, v := range s.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, env...)
	d := &scaleDaemon{cmd: cmd, done: make(chan struct{}), ready: map[string]time.Duration{}}
	pipe, err := cmd.StderrPipe()
	if err != nil {
		s.t.Fatal(err)
	}
	d.t0 = time.Now()
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	go func() {
		sc := bufio.NewScanner(pipe)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			d.mu.Lock()
			d.lines++
			if d.log.Len() < 4<<20 {
				d.log.WriteString(line + "\n")
			}
			if m := scaleReadyRe.FindStringSubmatch(line); m != nil {
				if _, ok := d.ready[m[1]]; !ok {
					d.ready[m[1]] = time.Since(d.t0)
				}
			}
			d.mu.Unlock()
		}
		err := cmd.Wait()
		if ee, ok := err.(*exec.ExitError); ok {
			d.exit = ee.ExitCode()
		}
		close(d.done)
	}()
	s.t.Cleanup(func() {
		select {
		case <-d.done:
		default:
			_ = cmd.Process.Kill()
			<-d.done
		}
	})
	return d
}

func (d *scaleDaemon) readyAt(svc string) (time.Duration, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.ready[svc]
	return v, ok
}

func (d *scaleDaemon) text() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.log.String()
}

func (d *scaleDaemon) pid() int { return d.cmd.Process.Pid }

// scaleResult is one run's numbers.
type scaleResult struct {
	Phase              string             `json:"phase"`
	PerType            int                `json:"channels_per_type"`
	Channels           int                `json:"channels"`
	Commit             string             `json:"commit"`
	ConfigureSec       float64            `json:"configure_seconds"`
	ValidateCold       []float64          `json:"config_validate_seconds_daemon_down"`
	ValidateWarm       []float64          `json:"config_validate_seconds_daemon_up"`
	ValidateCalls      map[string]int     `json:"provider_calls_during_3_warm_validates"`
	ValidateOut        int                `json:"config_validate_stdout_bytes"`
	ReadySec           map[string]float64 `json:"service_ready_seconds"`
	AllSyncedSec       float64            `json:"all_channels_first_sync_seconds"`
	SyncedCount        int                `json:"channels_first_synced"`
	StartupCalls       map[string]int     `json:"calls_until_all_synced"`
	WindowSec          float64            `json:"window_seconds"`
	WindowCalls        map[string]int     `json:"calls_in_window"`
	PerHour            map[string]float64 `json:"calls_per_hour_projected"`
	TokenByCred        map[string]int     `json:"token_calls_by_credential_max_min"`
	PeakInflight       int                `json:"fake_peak_inflight"`
	PeakPerSec         int                `json:"fake_peak_requests_in_one_second_window"`
	PerSecond          []int              `json:"fake_requests_per_second"`
	TopSecs            []string           `json:"fake_busiest_seconds"`
	Samples            []scaleSample      `json:"samples"`
	RSSPeakMB          float64            `json:"rss_peak_mb"`
	RSSIdleMB          float64            `json:"rss_last_mb"`
	HeapPeakMB         float64            `json:"heap_inuse_peak_mb"`
	HeapLastMB         float64            `json:"heap_inuse_last_mb"`
	GoroutinesMax      int                `json:"goroutines_max"`
	GoroutinesLast     int                `json:"goroutines_last"`
	FDsMax             int                `json:"open_fds_max"`
	FDsLast            int                `json:"open_fds_last"`
	DBTxPerHour        float64            `json:"db_transactions_per_hour_window"`
	DBTupWritesPerHour float64            `json:"db_tuple_writes_per_hour_window"`
	DBConnsMax         int                `json:"db_connections_max"`
	PoolWaitMax        int                `json:"pool_waiters_max"`
	TableDeltas        []string           `json:"db_table_activity_in_window"`
	GoroutineTop       []string           `json:"goroutine_top_stacks"`
	Status             map[string]any     `json:"service_status"`
	Receivers          int                `json:"receivers_reported"`
	DrainSec           float64            `json:"sigterm_to_exit_seconds"`
	ExitCode           int                `json:"exit_code"`
	LogLines           int                `json:"daemon_log_lines"`
	LogErrLines        int                `json:"daemon_log_error_lines"`
}

type scaleSample struct {
	PoolWait   int     `json:"pool_waiters"`
	DBConns    int     `json:"db_conns"`
	At         float64 `json:"t"`
	RSSMB      float64 `json:"rss_mb"`
	HeapInuse  float64 `json:"heap_inuse_mb"`
	Goroutines int     `json:"goroutines"`
	FDs        int     `json:"fds"`
}

func rssMB(pid int) float64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // harness
	if err != nil {
		return 0
	}
	kb, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return float64(kb) / 1024
}

func fdCount(pid int) int {
	out, err := exec.Command("lsof", "-nP", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // harness
	if err != nil && len(out) == 0 {
		return 0
	}
	return strings.Count(string(out), "\n") - 1
}

func debugStats(addr string) (heapInuse float64, goroutines int) {
	resp, err := http.Get("http://" + addr + "/stats") //nolint:gosec,noctx // loopback
	if err != nil {
		return 0, 0
	}
	defer func() { _ = resp.Body.Close() }()
	var m map[string]uint64
	if json.NewDecoder(resp.Body).Decode(&m) != nil {
		return 0, 0
	}
	return float64(m["heap_inuse"]) / (1 << 20), int(m["goroutines"])
}

// goroutineTop returns the most common goroutine stacks, by their
// first frame, from the pprof text dump.
func poolWaiters(addr string) int {
	resp, err := http.Get("http://" + addr + "/debug/pprof/goroutine?debug=1") //nolint:gosec,noctx // loopback
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	n := 0
	for _, blk := range strings.Split(string(b), "\n\n") {
		var c int
		if _, err := fmt.Sscanf(strings.TrimSpace(blk), "%d @", &c); err == nil && strings.Contains(blk, "puddle") {
			n += c
		}
	}
	return n
}

func dbStats(t *testing.T, name string) (tx, writes float64, conns int) {
	ctx := t.Context()
	_ = pg.admin.QueryRow(ctx, "SELECT xact_commit+xact_rollback, tup_inserted+tup_updated+tup_deleted FROM pg_stat_database WHERE datname=$1", name).Scan(&tx, &writes)
	_ = pg.admin.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=$1", name).Scan(&conns)
	return
}

// tableStats is each table's reads and writes so far, by relation.
func tableStats(t *testing.T, db *testDB) map[string][3]int64 {
	out := map[string][3]int64{}
	rows, err := db.pool().Query(t.Context(), "SELECT relname, coalesce(seq_scan,0)+coalesce(idx_scan,0), n_tup_ins, n_tup_upd+n_tup_del FROM pg_stat_user_tables")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		var a, b, c int64
		if rows.Scan(&n, &a, &b, &c) == nil {
			out[n] = [3]int64{a, b, c}
		}
	}
	return out
}

func goroutineTop(addr string) []string {
	resp, err := http.Get("http://" + addr + "/debug/pprof/goroutine?debug=1") //nolint:gosec,noctx // loopback
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	type grp struct {
		n     int
		stack string
	}
	var gs []grp
	for _, blk := range strings.Split(string(b), "\n\n") {
		blk = strings.TrimSpace(blk)
		var n int
		if _, err := fmt.Sscanf(blk, "%d @", &n); err != nil {
			continue
		}
		lines := strings.Split(blk, "\n")
		var frames []string
		for _, l := range lines[1:] {
			if i := strings.Index(l, "#\t"); i >= 0 {
				fr := strings.Fields(l[i+2:])
				if len(fr) >= 2 {
					frames = append(frames, fr[1])
				}
			}
			if len(frames) == 3 {
				break
			}
		}
		gs = append(gs, grp{n, strings.Join(frames, " < ")})
	}
	sort.Slice(gs, func(i, j int) bool { return gs[i].n > gs[j].n })
	var out []string
	for i := 0; i < len(gs) && i < 12; i++ {
		out = append(out, fmt.Sprintf("%d x %s", gs[i].n, gs[i].stack))
	}
	return out
}

var scaleBin = func() func() (string, error) {
	var once sync.Once
	var out string
	var err error
	return func() (string, error) {
		once.Do(func() {
			out = filepath.Join(filepath.Dir(heoBin), "heo-scale")
			b := exec.Command("go", "build", "-buildvcs=false", "-tags", "heoscale", "-o", out, "hop.top/heo") //nolint:gosec // harness
			b.Stdout, b.Stderr = os.Stderr, os.Stderr
			err = b.Run()
		})
		return out, err
	}
}()

func scaleSizes(t *testing.T) []int {
	v := os.Getenv("HEO_SCALE_SIZES")
	if v == "" {
		v = "5,50,100"
	}
	var out []int
	for _, f := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 {
			t.Fatalf("HEO_SCALE_SIZES %q", v)
		}
		out = append(out, n)
	}
	return out
}

func scaleWindow() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("HEO_SCALE_WINDOW")); err == nil && d > 0 {
		return d
	}
	return 60 * time.Second
}

func TestScaleMailChannels(t *testing.T) {
	if os.Getenv(scaleEnv) != "1" {
		t.Skip("scale harness: set HEO_SCALE=1 and -tags heoscale")
	}
	phases := []string{"default", "accel", "broken"}
	if v := os.Getenv("HEO_SCALE_PHASES"); v != "" {
		phases = strings.Split(v, ",")
	}
	for _, n := range scaleSizes(t) {
		for _, ph := range phases {
			t.Run(fmt.Sprintf("%s/%dx2", ph, n), func(t *testing.T) {
				broken := 0
				if ph == "broken" {
					broken = max(1, n/10)
				}
				runScale(t, n, ph == "accel", broken)
			})
		}
	}
}

// writeGrants stores one refresh-token grant per credential reference,
// as a business with a consent per mailbox would.
func (s *sandbox) writeGrants(tokenURL string, refs []string) {
	s.t.Helper()
	root := s.path("data", "heo", "default", "secrets", "uma", "default")
	if err := os.MkdirAll(root, 0o700); err != nil {
		s.t.Fatal(err)
	}
	for _, ref := range refs {
		grant, _ := json.Marshal(map[string]any{
			"v": 1, "kind": "refresh_token", "token_url": tokenURL, "client_id": ref,
			"scopes": []string{"mail"}, "secret_ref": "uma/default/" + ref + "-rt",
		})
		rt, _ := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": "rt-" + ref})
		// Graph's change-notification client state, a plain secret under
		// its own purpose; only the microsoft.mail references read it.
		ns, _ := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": "state-" + ref})
		files := map[string][]byte{ref: grant, ref + "-rt": rt}
		if strings.HasPrefix(ref, "ms-") {
			files[ref+"@"+microsoft.PurposeNotifications] = ns
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(root, name), body, 0o600); err != nil {
				s.t.Fatal(err)
			}
		}
	}
}

func timed(s *sandbox, args ...string) (result, time.Duration) {
	s.t.Helper()
	t0 := time.Now()
	r := s.run(nil, args...)
	return r, time.Since(t0)
}

func runScale(t *testing.T, perType int, accel bool, broken int) {
	bin, err := scaleBin()
	if err != nil {
		t.Fatalf("build scale heo: %v", err)
	}
	phase := "default"
	if accel {
		phase = "accel"
	}
	if broken > 0 {
		phase = "broken"
	}
	res := &scaleResult{Phase: phase, PerType: perType, Channels: 2 * perType, Commit: gitHead(), ReadySec: map[string]float64{}}
	out := os.Getenv("HEO_SCALE_OUT")
	if out == "" {
		out = t.TempDir()
	}
	_ = os.MkdirAll(out, 0o755)
	tag := fmt.Sprintf("%s-%03d", phase, 2*perType)

	db := newDB(t).migrate()
	fake := newScaleFake(accel)
	defer fake.close()

	// Channels: one credential and one mailbox each.
	hookURL := "https://hooks.example.com/heo/webhooks/"
	var chans []model.Channel
	var refs []string
	for i := 0; i < perType; i++ {
		ref := fmt.Sprintf("gm-%03d", i)
		user := fmt.Sprintf("gm%03d@example.com", i)
		cfg := google.Config{User: user, Topic: "projects/heo-test/topics/gmail-push",
			PushAudience: hookURL + "gmail", PushServiceAccount: "push@heo-test.iam.gserviceaccount.com"}
		if accel {
			cfg.PollIntervalMS = accelPollMS
		}
		chans = append(chans, model.Channel{Key: ref, Type: google.Type, CredentialRef: ref, Configuration: extension(t, google.ConfigSchema, cfg)})
		refs = append(refs, ref)
	}
	for i := 0; i < perType; i++ {
		ref := fmt.Sprintf("ms-%03d", i)
		cfg := microsoft.Config{Mailbox: fmt.Sprintf("ms%03d@example.com", i), NotificationURL: hookURL + "outlook"}
		if accel {
			cfg.PollIntervalMS = accelPollMS
			cfg.SubscriptionLifetimeMS = int64((45 * time.Minute) / time.Millisecond)
		}
		chans = append(chans, model.Channel{Key: ref, Type: microsoft.Type, CredentialRef: ref, Configuration: extension(t, microsoft.ConfigSchema, cfg)})
		refs = append(refs, ref)
	}
	t0 := time.Now()
	db.configure("default", chans...)
	res.ConfigureSec = time.Since(t0).Seconds()

	s := newSandbox(t)
	s.useStore(db)
	// The first `broken` mailboxes of each type have no credential stored.
	var granted []string
	for i, ref := range refs {
		if i%perType >= broken {
			granted = append(granted, ref)
		}
	}
	s.writeGrants(fake.tokenURL(), granted)
	// validate reads the store without a remote probe, so it finds only
	// the credential a channel's configuration makes necessary: the
	// microsoft channels set up for push need graph.notifications, and a
	// gmail channel has no conditional credential. Status reads every
	// broken channel's missing credential as a failed receiver.
	brokenRefs, validateRefs := map[string]bool{}, map[string]bool{}
	for i := 0; i < broken; i++ {
		brokenRefs[fmt.Sprintf("gm-%03d", i)], brokenRefs[fmt.Sprintf("ms-%03d", i)] = true, true
		validateRefs[fmt.Sprintf("ms-%03d", i)] = true
	}

	// config validate with the daemon down.
	for i := 0; i < 3; i++ {
		r, d := timed(s, "config", "validate")
		s.want(r, 0, "config", "validate")
		res.ValidateCold = append(res.ValidateCold, d.Seconds())
		res.ValidateOut = len(r.stdout)
		if i == 0 {
			_ = os.WriteFile(filepath.Join(out, tag+"-config-validate.txt"), []byte(r.stdout+r.stderr), 0o644)
		}
	}

	dbg, hook := freeAddr(t), freeAddr(t)
	env := []string{
		composition.ScaleGmailEnv + "=" + fake.gmailEndpoint(),
		composition.ScaleGraphEnv + "=" + fake.graphEndpoint(),
		composition.ScaleDebugEnv + "=" + dbg,
	}
	fake.mu.Lock()
	fake.t0 = time.Now() // fake time zero is the daemon start
	fake.mu.Unlock()
	args := []string{"-c", "services.webhook.addr=" + hook}
	sp := os.Getenv("HEO_SCALE_SPREAD")
	if sp == "" && broken > 0 {
		sp = "0s"
	}
	if sp != "" {
		args = append(args, "-c", "services.receivers.start_spread="+sp)
	}
	d := s.startScale(bin, env, append(args, "service", "start", "--enable", "webhook")...)

	var samples []scaleSample
	sample := func(withFD bool) {
		heap, gor := debugStats(dbg)
		_, _, conns := dbStats(t, db.name)
		sm := scaleSample{PoolWait: poolWaiters(dbg), DBConns: conns, At: time.Since(d.t0).Seconds(), RSSMB: rssMB(d.pid()), HeapInuse: heap, Goroutines: gor}
		if withFD {
			sm.FDs = fdCount(d.pid())
		}
		samples = append(samples, sm)
	}

	// Wait for the receivers service, then every channel's first sync.
	want := 2 * (perType - broken)
	deadline := time.Now().Add(10 * time.Minute) // a staggered start takes up to start_spread
	var lastFD time.Time
	allSynced := func() bool { n, _ := fake.synced(want); return n >= want }
	for !allSynced() {
		if time.Now().After(deadline) {
			n, _ := fake.synced(want)
			t.Logf("only %d/%d channels synced in 10m", n, want)
			break
		}
		select {
		case <-d.done:
			_ = os.WriteFile(filepath.Join(out, tag+"-daemon-exit.log"), []byte(d.text()), 0o644)
			t.Fatalf("daemon exited %d:\n%s", d.exit, tail(d.text(), 4000))
		default:
		}
		sample(time.Since(lastFD) > 5*time.Second)
		if time.Since(lastFD) > 5*time.Second {
			lastFD = time.Now()
		}
		time.Sleep(250 * time.Millisecond)
	}
	// Every channel has called once; the startup traffic is over when the
	// stand-ins have heard nothing for two seconds.
	for qd := time.Now().Add(2 * time.Minute); time.Now().Before(qd); {
		if fake.idle() > 2*time.Second {
			break
		}
		sample(false)
		time.Sleep(250 * time.Millisecond)
	}
	n, _ := fake.synced(want)
	res.SyncedCount, res.AllSyncedSec = n, fake.lastEvent().Seconds()
	for _, svc := range []string{"socket", "workers", "receivers", "webhook", "bridge"} {
		if at, ok := d.readyAt(svc); ok {
			res.ReadySec[svc] = at.Seconds()
		}
	}
	res.StartupCalls = fake.snapshot()
	startCalls := fake.snapshot()

	// Steady window.
	win := scaleWindow()
	wstart := time.Now()
	tx0, wr0, _ := dbStats(t, db.name)
	ts0 := tableStats(t, db)
	for time.Since(wstart) < win {
		sample(time.Since(lastFD) > 5*time.Second)
		if time.Since(lastFD) > 5*time.Second {
			lastFD = time.Now()
		}
		time.Sleep(250 * time.Millisecond)
	}
	res.WindowSec = time.Since(wstart).Seconds()
	tx1, wr1, _ := dbStats(t, db.name)
	for name, v := range tableStats(t, db) {
		d := [3]int64{v[0] - ts0[name][0], v[1] - ts0[name][1], v[2] - ts0[name][2]}
		if d != [3]int64{} {
			res.TableDeltas = append(res.TableDeltas, fmt.Sprintf("%s: reads=%d inserts=%d updates+deletes=%d", name, d[0], d[1], d[2]))
		}
	}
	sort.Strings(res.TableDeltas)
	res.DBTxPerHour, res.DBTupWritesPerHour = (tx1-tx0)*3600/res.WindowSec, (wr1-wr0)*3600/res.WindowSec
	endCalls := fake.snapshot()
	res.WindowCalls, res.PerHour = map[string]int{}, map[string]float64{}
	for k, v := range endCalls {
		res.WindowCalls[k] = v - startCalls[k]
		res.PerHour[k] = float64(v-startCalls[k]) * 3600 / res.WindowSec
	}
	res.GoroutineTop = goroutineTop(dbg)
	if resp, err := http.Get("http://" + dbg + "/debug/pprof/goroutine?debug=2"); err == nil { //nolint:gosec,noctx // loopback
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		_ = os.WriteFile(filepath.Join(out, tag+"-goroutines.txt"), b, 0o644)
	}

	if b, err := exec.Command("lsof", "-nP", "-p", strconv.Itoa(d.pid())).Output(); err == nil || len(b) > 0 { //nolint:gosec // harness
		_ = os.WriteFile(filepath.Join(out, tag+"-lsof.txt"), b, 0o644)
	}
	// status and validate against the live daemon.
	hr, hd := timed(s, "service", "status")
	s.want(hr, 0, "service", "status")
	jr, jd := timed(s, "service", "status", "--format", "json")
	s.want(jr, 0, "service", "status", "--format", "json")
	_ = os.WriteFile(filepath.Join(out, tag+"-status.txt"), []byte(hr.stdout), 0o644)
	_ = os.WriteFile(filepath.Join(out, tag+"-status.json"), []byte(jr.stdout), 0o644)
	res.Status = map[string]any{
		"human_seconds": hd.Seconds(), "human_bytes": len(hr.stdout), "human_lines": strings.Count(hr.stdout, "\n"),
		"human_max_line": maxLine(hr.stdout),
		"json_seconds":   jd.Seconds(), "json_bytes": len(jr.stdout), "json_lines": strings.Count(jr.stdout, "\n"),
		"stderr_bytes": len(hr.stderr),
	}
	var js struct {
		Sections []struct {
			Title string `json:"title"`
			Data  any    `json:"data"`
		} `json:"sections"`
	}
	if json.Unmarshal([]byte(jr.stdout), &js) == nil {
		var secs []string
		states := map[string]int{}
		for _, sec := range js.Sections {
			if m, ok := sec.Data.(map[string]any); ok && sec.Title == "receivers" {
				if sc, ok := m["scopes"].([]any); ok {
					for _, e := range sc {
						if em, ok := e.(map[string]any); ok {
							states[fmt.Sprint(em["state"])]++
						}
					}
				}
			}
			b, _ := json.Marshal(sec.Data)
			secs = append(secs, fmt.Sprintf("%s=%dB", sec.Title, len(b)))
		}
		res.Status["json_section_bytes"] = secs
		res.Status["receiver_states"] = states
	}
	if broken > 0 {
		checkBrokenStatus(t, jr.stdout, brokenRefs, want)
	}
	vBefore := fake.snapshot()
	for i := 0; i < 3; i++ {
		r, dd := timed(s, "config", "validate")
		if broken > 0 {
			// A missing credential is an error finding, so exit 2; the
			// findings name exactly the channels left without one that
			// validate can see.
			s.want(r, 2, "config", "validate")
			if got := validateErrorChannels(r.stdout); !reflect.DeepEqual(got, sortedRefs(validateRefs)) {
				t.Fatalf("config validate error findings name channels %v, want %v\nstdout: %s", got, sortedRefs(validateRefs), r.stdout)
			}
		} else {
			s.want(r, 0, "config", "validate")
		}
		res.ValidateWarm = append(res.ValidateWarm, dd.Seconds())
		if i == 0 {
			_ = os.WriteFile(filepath.Join(out, tag+"-config-validate-daemon-up.txt"), []byte(r.stdout+r.stderr), 0o644)
		}
	}
	res.ValidateCalls = map[string]int{}
	for k, v := range fake.snapshot() {
		if v != vBefore[k] {
			res.ValidateCalls[k] = v - vBefore[k]
		}
	}
	sample(true)

	// SIGTERM drain.
	t1 := time.Now()
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.done:
	case <-time.After(60 * time.Second):
		t.Errorf("daemon did not drain in 60s")
		_ = d.cmd.Process.Kill()
		<-d.done
	}
	res.DrainSec, res.ExitCode = time.Since(t1).Seconds(), d.exit
	log := d.text()
	d.mu.Lock()
	res.LogLines = d.lines
	d.mu.Unlock()
	res.LogErrLines = strings.Count(log, "level=ERROR") + strings.Count(log, "level=error") + strings.Count(log, " ERROR ")
	_ = os.WriteFile(filepath.Join(out, tag+"-daemon.log"), []byte(tail(log, 400_000)), 0o644)
	res.Receivers = strings.Count(log, "runtime: receiver ready scope=")
	res.Samples = samples
	for _, sm := range samples {
		res.RSSPeakMB = max(res.RSSPeakMB, sm.RSSMB)
		res.HeapPeakMB = max(res.HeapPeakMB, sm.HeapInuse)
		res.GoroutinesMax = max(res.GoroutinesMax, sm.Goroutines)
		res.FDsMax = max(res.FDsMax, sm.FDs)
		res.DBConnsMax = max(res.DBConnsMax, sm.DBConns)
		res.PoolWaitMax = max(res.PoolWaitMax, sm.PoolWait)
		if sm.RSSMB > 0 {
			res.RSSIdleMB, res.HeapLastMB, res.GoroutinesLast = sm.RSSMB, sm.HeapInuse, sm.Goroutines
		}
		if sm.FDs > 0 {
			res.FDsLast = sm.FDs
		}
	}
	fake.mu.Lock()
	res.PeakInflight = fake.maxInflight
	perSec := map[int]int{}
	for _, e := range fake.events {
		perSec[int(e.at/time.Second)]++
	}
	for sec, n := range perSec {
		for len(res.PerSecond) <= sec {
			res.PerSecond = append(res.PerSecond, 0)
		}
		res.PerSecond[sec] = n
	}
	type kv struct{ sec, n int }
	var kvs []kv
	for k, v := range perSec {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].n > kvs[j].n })
	for i := 0; i < len(kvs) && i < 8; i++ {
		res.TopSecs = append(res.TopSecs, fmt.Sprintf("t+%ds: %d requests", kvs[i].sec, kvs[i].n))
	}
	if len(kvs) > 0 {
		res.PeakPerSec = kvs[0].n
	}
	lo, hi := 1<<30, 0
	for _, c := range fake.tokenBy {
		lo, hi = min(lo, c), max(hi, c)
	}
	res.TokenByCred = map[string]int{"credentials": len(fake.tokenBy), "max": hi, "min": lo}
	fake.mu.Unlock()

	b, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(filepath.Join(out, tag+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: ready=%v allSynced=%.1fs rssPeak=%.0fMB heap=%.0fMB goroutines=%d fds=%d startup=%v window=%v", tag,
		res.ReadySec, res.AllSyncedSec, res.RSSPeakMB, res.HeapPeakMB, res.GoroutinesMax, res.FDsMax, res.StartupCalls, res.WindowCalls)
	_ = net.IPv4len
}

// validateErrorChannels is the sorted, distinct channel keys of the
// error rows in `config validate` table output (KEY reads
// channel:<ref> or channel:<ref> credential:<purpose>).
func validateErrorChannels(out string) []string {
	set := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) > 1 && f[0] == "error" {
			if ref, ok := strings.CutPrefix(f[1], "channel:"); ok {
				set[ref] = true
			} else {
				set["<"+f[1]+">"] = true // an error that names no channel
			}
		}
	}
	return sortedRefs(set)
}

func sortedRefs(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// checkBrokenStatus asserts the receivers section of `service status
// --format json`: degraded, with exactly the channels that have no
// credential failed (not_found), every other receiver ready, none still
// starting.
func checkBrokenStatus(t *testing.T, statusJSON string, brokenRefs map[string]bool, ready int) {
	t.Helper()
	var js struct {
		Sections []struct {
			Title  string `json:"title"`
			Status string `json:"status"`
			Data   struct {
				State  string `json:"state"`
				Scopes []struct {
					Key    string `json:"key"`
					State  string `json:"state"`
					Reason string `json:"reason"`
				} `json:"scopes"`
			} `json:"data"`
		} `json:"sections"`
	}
	if err := json.Unmarshal([]byte(statusJSON), &js); err != nil {
		t.Fatalf("status json: %v", err)
	}
	found := false
	for _, sec := range js.Sections {
		if sec.Title != "receivers" {
			continue
		}
		found = true
		if sec.Data.State != "degraded" {
			t.Errorf("receivers state %q, want degraded", sec.Data.State)
		}
		failed, nready := map[string]bool{}, 0
		for _, sc := range sec.Data.Scopes {
			switch sc.State {
			case "ready":
				nready++
			case "failed":
				failed[sc.Key] = true
				if !strings.Contains(sc.Reason, "credential not found") {
					t.Errorf("receiver %s failed with reason %q, want credential not found", sc.Key, sc.Reason)
				}
			default:
				t.Errorf("receiver %s state %q (%s), want ready or failed", sc.Key, sc.State, sc.Reason)
			}
		}
		if !reflect.DeepEqual(sortedRefs(failed), sortedRefs(brokenRefs)) {
			t.Errorf("failed receivers %v, want %v", sortedRefs(failed), sortedRefs(brokenRefs))
		}
		if nready != ready {
			t.Errorf("%d receivers ready, want %d", nready, ready)
		}
	}
	if !found {
		t.Errorf("no receivers section in status")
	}
}

func maxLine(s string) int {
	m := 0
	for _, l := range strings.Split(s, "\n") {
		m = max(m, len([]rune(l)))
	}
	return m
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func gitHead() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
