package daemon_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"hop.top/heo/internal/composition"
	"hop.top/uma/ports"
)

// discordAPI is Discord, in process: the REST API the discord.chat adapter
// calls and the gateway it holds a WebSocket to, behind one httptest TLS
// server of the test's own. heo's HTTP client (composition.NewHTTPClient,
// network policy guard included) reaches it through a transport that
// sends every host to the server, so the WebSocket upgrade crosses the
// seam production uses. Nothing reaches the network. It answers the
// documented shapes of the routes the adapter calls (messages, reactions,
// typing, Get Gateway Bot, interaction callbacks and webhooks) and the
// gateway of "Gateway" and "Gateway Events": Hello, Heartbeat ACK,
// Identify answered with READY, Resume answered with what the session
// missed and RESUMED, and closes with the codes a test chooses.
type discordAPI struct {
	t       *testing.T
	srv     *httptest.Server
	token   string
	botUser string
	app     string

	mu    sync.Mutex
	calls []discordCall
	seq   uint64
	// msgs holds the messages posted, by "<channel>:<id>".
	msgs map[string]*discordPosted
	// threads maps a thread channel to its parent; any other channel is
	// a text channel.
	threads map[string]string
	// originals holds each interaction token's original response.
	originals map[string]*discordPosted
	// dms maps a user to the direct message channel Create DM opened.
	dms map[string]string
	// startsLeft is session_start_limit.remaining; resetAfter its reset.
	startsLeft int
	resetAfter time.Duration

	// The gateway.
	heartbeat    time.Duration
	closeOnReady int
	noAck        bool
	sessions     map[string]*discordSession
	identifies   []map[string]any
	resumes      []map[string]any
	conns        []*discordConn
	connected    chan *discordConn
}

// discordCall is one REST request the adapter made.
type discordCall struct {
	Method string
	Path   string // after /api/v10/
	Host   string // the host the adapter addressed
	Auth   string
	Agent  string
	Body   map[string]any
}

type discordPosted struct {
	Channel   string
	ID        string
	Author    string
	Text      string
	Flags     int
	Reactions []string
	Deleted   bool
}

// discordSession is a gateway session: what a Resume can replay.
type discordSession struct {
	id  string
	seq int64
	log []discordDispatch
}

type discordDispatch struct {
	seq  int64
	typ  string
	data json.RawMessage
}

// discordConn is one gateway connection, from Discord's side.
type discordConn struct {
	api  *discordAPI
	ws   *websocket.Conn
	done chan struct{}

	mu         sync.Mutex
	sess       *discordSession
	heartbeats []any
	closeCode  websocket.StatusCode // how the client ended it; -1 until it did
}

const (
	discordFakeHost    = "discord.test"
	discordGatewayHost = "gateway.discord.test"
	discordResumeHost  = "resume.discord.test"
)

func newDiscordAPI(t *testing.T, token, botUser, app string) *discordAPI {
	t.Helper()
	d := &discordAPI{
		t: t, token: token, botUser: botUser, app: app,
		msgs: map[string]*discordPosted{}, threads: map[string]string{}, originals: map[string]*discordPosted{},
		dms: map[string]string{}, startsLeft: 1000, resetAfter: time.Hour,
		heartbeat: time.Second, sessions: map[string]*discordSession{}, connected: make(chan *discordConn, 64),
	}
	d.srv = httptest.NewTLSServer(http.HandlerFunc(d.serve))
	t.Cleanup(func() {
		d.srv.CloseClientConnections()
		d.srv.Close()
	})
	return d
}

// client is heo's HTTP client over a transport that sends every host to
// the in-process server: the seam the adapter's REST calls and its
// WebSocket dial both cross.
func (d *discordAPI) client() ports.HTTPClient {
	return composition.NewHTTPClient(discordTransport{d: d})
}

type discordTransport struct{ d *discordAPI }

func (t discordTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	out := r.Clone(r.Context())
	out.URL.Scheme, out.URL.Host, out.Host = "https", t.d.srv.Listener.Addr().String(), ""
	out.Header.Set("X-Fake-Host", r.URL.Host)
	base := t.d.srv.Client().Transport.(*http.Transport)
	return base.RoundTrip(out)
}

// apiURL and gatewayURL are what the channel's configuration names.
func (d *discordAPI) apiURL() string     { return "https://" + discordFakeHost + "/api/v10/" }
func (d *discordAPI) gatewayURL() string { return "wss://" + discordGatewayHost }

func (d *discordAPI) serve(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		d.serveGateway(w, r)
		return
	}
	d.serveREST(w, r)
}

func writeDiscord(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func discordError(w http.ResponseWriter, status, code int) {
	writeDiscord(w, status, map[string]any{"code": code, "message": "error"})
}

func (d *discordAPI) message(m *discordPosted) map[string]any {
	return map[string]any{
		"id": m.ID, "channel_id": m.Channel, "content": m.Text, "type": 0, "flags": m.Flags,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"author":    map[string]any{"id": m.Author, "bot": m.Author == d.botUser},
	}
}

func (d *discordAPI) serveREST(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			d.t.Errorf("discord request %s is not JSON: %v", r.URL.Path, err)
		}
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v10/")
	call := discordCall{
		Method: r.Method, Path: path, Host: r.Header.Get("X-Fake-Host"), Auth: r.Header.Get("Authorization"),
		Agent: r.Header.Get("User-Agent"), Body: body,
	}
	d.mu.Lock()
	d.calls = append(d.calls, call)
	d.mu.Unlock()
	if call.Host != discordFakeHost {
		http.Error(w, "unexpected host "+call.Host, http.StatusBadGateway)
		return
	}
	// Interaction callbacks and webhooks carry their token in the path;
	// every other route wants the bot token.
	tokenInPath := strings.HasPrefix(path, "interactions/") || strings.HasPrefix(path, "webhooks/")
	if !tokenInPath && call.Auth != "Bot "+d.token {
		discordError(w, http.StatusUnauthorized, 0)
		return
	}
	parts := strings.Split(path, "/")
	str := func(k string) string { v, _ := body[k].(string); return v }
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case path == "gateway/bot":
		writeDiscord(w, http.StatusOK, map[string]any{
			"url": "wss://" + discordGatewayHost, "shards": 1,
			"session_start_limit": map[string]any{
				"total": 1000, "remaining": d.startsLeft, "reset_after": d.resetAfter.Milliseconds(), "max_concurrency": 1,
			},
		})
	case path == "users/@me/channels" && r.Method == http.MethodPost:
		user := str("recipient_id")
		if d.dms[user] == "" {
			d.dms[user] = strconv.Itoa(900000000000000100 + len(d.dms))
		}
		writeDiscord(w, http.StatusOK, map[string]any{"id": d.dms[user], "type": 1})
	case len(parts) == 2 && parts[0] == "channels" && r.Method == http.MethodGet:
		ch := map[string]any{"id": parts[1], "type": 0}
		if parent := d.threads[parts[1]]; parent != "" {
			ch["type"], ch["parent_id"] = 11, parent
		}
		writeDiscord(w, http.StatusOK, ch)
	case len(parts) == 3 && parts[0] == "channels" && parts[2] == "messages" && r.Method == http.MethodPost:
		d.seq++
		m := &discordPosted{Channel: parts[1], ID: strconv.FormatUint(uint64(1_000_000_000_000_000_000)+d.seq, 10), Author: d.botUser, Text: str("content")}
		d.msgs[m.Channel+":"+m.ID] = m
		writeDiscord(w, http.StatusOK, d.message(m))
	case len(parts) == 4 && parts[0] == "channels" && parts[2] == "messages":
		m := d.msgs[parts[1]+":"+parts[3]]
		switch {
		case m == nil || m.Deleted:
			discordError(w, http.StatusNotFound, 10008)
		case r.Method == http.MethodPatch && m.Author != d.botUser:
			discordError(w, http.StatusForbidden, 50005)
		case r.Method == http.MethodPatch:
			m.Text = str("content")
			writeDiscord(w, http.StatusOK, d.message(m))
		case r.Method == http.MethodDelete:
			m.Deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			writeDiscord(w, http.StatusOK, d.message(m))
		}
	case len(parts) == 7 && parts[0] == "channels" && parts[2] == "messages" && parts[4] == "reactions" && parts[6] == "@me":
		m := d.msgs[parts[1]+":"+parts[3]]
		if m == nil {
			discordError(w, http.StatusNotFound, 10008)
			return
		}
		emoji := parts[5]
		if r.Method == http.MethodPut {
			m.Reactions = append(m.Reactions, emoji)
		} else {
			m.Reactions = slices.DeleteFunc(m.Reactions, func(e string) bool { return e == emoji })
		}
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 4 && parts[0] == "channels" && parts[2] == "thread-members" && (r.Method == http.MethodPut || r.Method == http.MethodDelete):
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 3 && parts[0] == "channels" && parts[2] == "typing":
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 4 && parts[0] == "interactions" && parts[3] == "callback":
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 3 && parts[0] == "webhooks" && r.Method == http.MethodPost:
		d.seq++
		m := &discordPosted{Channel: "followup", ID: strconv.FormatUint(uint64(1_000_000_000_000_000_000)+d.seq, 10), Author: d.app, Text: str("content")}
		if f, ok := body["flags"].(float64); ok {
			m.Flags = int(f)
		}
		d.msgs[parts[2]+":"+m.ID] = m
		writeDiscord(w, http.StatusOK, d.message(m))
	case len(parts) == 5 && parts[0] == "webhooks" && parts[3] == "messages" && parts[4] == "@original":
		m := d.originals[parts[2]]
		if m == nil {
			m = &discordPosted{Channel: "original", ID: "1000000000000000999", Author: d.app}
			d.originals[parts[2]] = m
		}
		if r.Method == http.MethodPatch {
			m.Text = str("content")
			m.Flags = 0
		}
		writeDiscord(w, http.StatusOK, d.message(m))
	default:
		discordError(w, http.StatusNotFound, 0)
	}
}

// called returns the REST requests made with method to the path prefix,
// in order.
func (d *discordAPI) called(method, prefix string) []discordCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []discordCall
	for _, c := range d.calls {
		if c.Method == method && strings.HasPrefix(c.Path, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// posted is the message "<channel>:<id>" as Discord holds it.
func (d *discordAPI) posted(id string) *discordPosted {
	d.mu.Lock()
	defer d.mu.Unlock()
	if m := d.msgs[id]; m != nil {
		c := *m
		c.Reactions = slices.Clone(m.Reactions)
		return &c
	}
	return nil
}

// person puts a message of a person in Discord, as if written earlier.
func (d *discordAPI) person(channel, id, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.msgs[channel+":"+id] = &discordPosted{Channel: channel, ID: id, Author: discordPerson, Text: text}
}

func (d *discordAPI) setThread(channel, parent string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.threads[channel] = parent
}

func (d *discordAPI) setStarts(left int, reset time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.startsLeft, d.resetAfter = left, reset
}

// The gateway.

func (d *discordAPI) serveGateway(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("v") != "10" || r.URL.Query().Get("encoding") != "json" {
		http.Error(w, "unknown gateway version", http.StatusNotFound)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	c := &discordConn{api: d, ws: ws, done: make(chan struct{}), closeCode: -1}
	d.mu.Lock()
	d.conns = append(d.conns, c)
	interval := d.heartbeat
	d.mu.Unlock()
	defer close(c.done)
	_ = c.send(10, map[string]any{"heartbeat_interval": interval.Milliseconds()}, nil, "")
	for {
		_, data, err := ws.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.closeCode = websocket.CloseStatus(err)
			c.mu.Unlock()
			return
		}
		var p struct {
			Op int             `json:"op"`
			D  json.RawMessage `json:"d"`
		}
		if json.Unmarshal(data, &p) != nil {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(p.D, &m)
		switch p.Op {
		case 1:
			var seq any
			_ = json.Unmarshal(p.D, &seq)
			c.mu.Lock()
			c.heartbeats = append(c.heartbeats, seq)
			c.mu.Unlock()
			d.mu.Lock()
			ack := !d.noAck
			d.mu.Unlock()
			if ack {
				_ = c.send(11, nil, nil, "")
			}
		case 2:
			d.identify(c, m)
		case 6:
			d.resume(c, m)
		}
	}
}

func (d *discordAPI) identify(c *discordConn, m map[string]any) {
	d.mu.Lock()
	d.identifies = append(d.identifies, m)
	code := d.closeOnReady
	d.mu.Unlock()
	if m["token"] != d.token {
		_ = c.ws.Close(4004, "Authentication failed.")
		return
	}
	if code != 0 {
		_ = c.ws.Close(websocket.StatusCode(code), "closed by the fake")
		return
	}
	d.mu.Lock()
	s := &discordSession{id: "session-" + strconv.Itoa(len(d.sessions)+1)}
	d.sessions[s.id] = s
	d.mu.Unlock()
	c.mu.Lock()
	c.sess = s
	c.mu.Unlock()
	c.dispatchLogged("READY", map[string]any{
		"v": 10, "session_id": s.id, "resume_gateway_url": "wss://" + discordResumeHost,
		"user": map[string]any{"id": d.botUser, "bot": true}, "application": map[string]any{"id": d.app}, "guilds": []any{},
	}, false)
	d.ready(c)
}

func (d *discordAPI) resume(c *discordConn, m map[string]any) {
	d.mu.Lock()
	d.resumes = append(d.resumes, m)
	id, _ := m["session_id"].(string)
	s := d.sessions[id]
	d.mu.Unlock()
	seq, _ := m["seq"].(float64)
	if s == nil || m["token"] != d.token {
		_ = c.send(9, false, nil, "")
		return
	}
	c.mu.Lock()
	c.sess = s
	c.mu.Unlock()
	d.mu.Lock()
	var replay []discordDispatch
	for _, l := range s.log {
		if l.seq > int64(seq) {
			replay = append(replay, l)
		}
	}
	d.mu.Unlock()
	for _, l := range replay {
		seq := l.seq
		_ = c.send(0, l.data, &seq, l.typ)
	}
	c.dispatchLogged("RESUMED", map[string]any{}, false)
	d.ready(c)
}

func (d *discordAPI) ready(c *discordConn) {
	select {
	case d.connected <- c:
	default:
	}
}

func (c *discordConn) send(op int, v any, s *int64, typ string) error {
	raw := json.RawMessage("null")
	if v != nil {
		if r, ok := v.(json.RawMessage); ok {
			raw = r
		} else {
			raw, _ = json.Marshal(v)
		}
	}
	b, _ := json.Marshal(map[string]any{"op": op, "d": raw, "s": s, "t": typ})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// dispatchLogged sends a dispatch with the session's next sequence
// number, logging it for replay when log is set.
func (c *discordConn) dispatchLogged(typ string, v any, log bool) int64 {
	raw, _ := json.Marshal(v)
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	c.api.mu.Lock()
	s.seq++
	seq := s.seq
	if log {
		s.log = append(s.log, discordDispatch{seq: seq, typ: typ, data: raw})
	}
	c.api.mu.Unlock()
	_ = c.send(0, json.RawMessage(raw), &seq, typ)
	return seq
}

func (c *discordConn) dispatch(typ string, v any) int64 { return c.dispatchLogged(typ, v, true) }

func (c *discordConn) closeWith(code int) { _ = c.ws.Close(websocket.StatusCode(code), "") }

func (c *discordConn) ended(d time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (c *discordConn) how() websocket.StatusCode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeCode
}

// next waits for the next connection to reach READY or RESUMED.
func (d *discordAPI) next() *discordConn {
	d.t.Helper()
	select {
	case c := <-d.connected:
		return c
	case <-time.After(5 * time.Second):
		d.t.Fatal("no gateway session")
	}
	return nil
}

func (d *discordAPI) counts() (identifies, resumes, conns int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.identifies), len(d.resumes), len(d.conns)
}

// dispatchOffline logs a dispatch for the newest session with no
// connection to send it on, as Discord holds what a bot missed while it
// was away, for a Resume to replay.
func (d *discordAPI) dispatchOffline(typ string, v any) {
	raw, _ := json.Marshal(v)
	d.mu.Lock()
	defer d.mu.Unlock()
	var s *discordSession
	for _, x := range d.sessions {
		if s == nil || x.id > s.id {
			s = x
		}
	}
	if s == nil {
		d.t.Fatal("no session to log a dispatch for")
	}
	s.seq++
	s.log = append(s.log, discordDispatch{seq: s.seq, typ: typ, data: raw})
}

// total is how many REST requests the adapter made.
func (d *discordAPI) total() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// dmOf is the direct message channel Create DM opened for user.
func (d *discordAPI) dmOf(user string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dms[user]
}
