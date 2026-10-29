package main

// Embedded playback: the companion turns its machine's mpv decode of the
// original file into a fragmented-MP4 stream that the web app's own <video>
// element plays INSIDE the existing page — no second window ever opens.
//
// Safety model (must never regress):
//   - The local server binds 127.0.0.1 on an ephemeral port. It never binds a
//     wildcard or non-loopback address, so nothing off-box can reach it.
//   - Every request must carry a per-session random token (crypto/rand) in the
//     URL path. The token lives only in companion memory — never on disk,
//     never in argv. A page that guesses the port still gets nothing.
//   - Only pages from the PAIRED SERVER's origin may consume the stream or
//     send control commands: the Origin header is checked against the paired
//     server's scheme+host on every request.
//   - Chrome's Private Network Access is honored: preflight gets
//     Access-Control-Allow-Private-Network: true alongside the origin-scoped
//     CORS headers, so the browser can lawfully bridge public page → loopback.
//   - The upstream source is strictly the paired server's device-authorized
//     stream (sameServer check as windowed playback). The device token is
//     attached as an HTTP header configured over mpv's private IPC socket —
//     never in argv, never in a URL.
//   - Control and stream endpoints are method-restricted, body-size-capped,
//     and idle-bounded. When the tab goes away or playback ends, the feeder
//     mpv and the listener are torn down; nothing keeps listening.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	embeddedMaxSessions   = 2                // one active + one warm-over; excess reaps the oldest
	embeddedOrphanTimeout = 90 * time.Second // never attached → reap (slow pages allowed)
	embeddedIdleTimeout   = 90 * time.Second // attached, then stopped consuming → reap
	embeddedProgressEvery = 5                // progress POSTs every Nth monitor tick (2s ticks)
	embeddedIdleWait      = 100 * time.Millisecond
)

// embeddedSession is one "play in the page" playback: a loopback listener plus
// at most one encode-mode mpv (the feeder) at a time.
type feederProcess interface {
	close()
}

type embeddedSession struct {
	mu       sync.Mutex
	feederMu sync.Mutex

	token string // path secret, per session
	port  int    // bound loopback port, 0 until listening
	srv   *http.Server
	ln    net.Listener

	itemID    string
	channelID string
	server    string // paired server base URL
	origin    string // allowed page origin (scheme+host of server)
	srcURL    string // device-authorized upstream stream URL (header-authed)
	srcToken  string // device token for the Authorization header
	title     string
	audioOnly bool

	// live state
	feeder         feederProcess
	sock           string
	paused         bool
	disconnectedAt time.Time
	start          float64 // position the next feeder should begin at
	position       float64
	duration       float64
	ended          bool
	clients        int // attached stream readers
	born           time.Time

	stopOnce sync.Once
	done     chan struct{} // closed when fully torn down
}

// feeder is one encode-mode mpv writing fMP4 to a pipe.
type feeder struct {
	cmd       *exec.Cmd
	stdout    io.ReadCloser
	sock      string
	err       *bytes.Buffer
	closeOnce sync.Once
}

// failErr returns the last mpv log lines for error reporting.
func (f *feeder) failErr(context string) error {
	msg := ""
	if f.err != nil {
		msg = f.err.String()
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
	}
	if msg == "" {
		return errors.New(context)
	}
	return fmt.Errorf("%s: %s", context, redactLog(msg))
}

func (f *feeder) close() {
	if f == nil {
		return
	}
	f.closeOnce.Do(func() {
		if f.cmd == nil {
			_ = os.Remove(f.sock)
			_ = os.Remove(f.sock + ".log")
			return
		}
		if f.cmd.Process == nil {
			return
		}
		_ = ipc(f.sock, []any{"quit"})
		terminateProcess(f.cmd)
		_ = f.stdout.Close()
		_ = f.cmd.Wait()
		_ = os.Remove(f.sock)
	})
}

func (s *embeddedSession) noteClient(delta int) {
	s.mu.Lock()
	s.clients += delta
	if s.clients < 0 {
		s.clients = 0
	}
	if s.clients == 0 {
		s.disconnectedAt = time.Now()
	} else {
		s.disconnectedAt = time.Time{}
	}
	s.mu.Unlock()
}

func (s *embeddedSession) finishFeeder(f feederProcess, err error) {
	s.mu.Lock()
	if s.feeder == f {
		if errors.Is(err, io.EOF) {
			s.ended = true
		} else if err != nil {
			s.feeder = nil
			f.close()
		}
	}
	s.mu.Unlock()
}

func (s *embeddedSession) stopped() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// embedded manages the at-most-N live sessions.
type embedded struct {
	mu       sync.Mutex
	sessions []*embeddedSession
}

var embeddedPlayers = &embedded{}

// startEmbedded validates mpv, binds the loopback relay, and returns the local
// stream URL + public session id. mpv itself starts when the page first asks
// for /stream — one encode process, no local-audio double play.
func startEmbedded(c config, itemID, channelID, streamPath, title string, start float64, audioOnly bool) (string, string, error) {
	playbackLifecycle.RLock()
	defer playbackLifecycle.RUnlock()
	if loadConfig().Token == "" {
		return "", "", errors.New("companion is not paired")
	}
	if !mpvAvailable() {
		return "", "", errors.New("mpv is not installed")
	}
	e := embeddedPlayers
	e.mu.Lock()
	live := e.sessions[:0]
	for _, s := range e.sessions {
		select {
		case <-s.done:
		default:
			live = append(live, s)
		}
	}
	e.sessions = live
	if len(live) >= embeddedMaxSessions {
		live[0].stop() // oldest first: a leaked session must not wedge new ones
		live = live[1:]
	}
	e.sessions = live
	e.mu.Unlock()

	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		return "", "", err
	}
	s := &embeddedSession{
		token:     hex.EncodeToString(tok),
		itemID:    itemID,
		channelID: channelID,
		server:    c.Server,
		origin:    serverOrigin(c.Server),
		srcURL:    streamPath,
		srcToken:  c.Token,
		title:     title,
		audioOnly: audioOnly,
		start:     start,
		born:      time.Now(),
		done:      make(chan struct{}),
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("loopback bind: %w", err)
	}
	s.port = ln.Addr().(*net.TCPAddr).Port
	s.ln = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/stream/"+s.token, s.serveStream)
	mux.HandleFunc("/control/"+s.token, s.serveControl)
	s.srv = &http.Server{
		Handler:           s.guard(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() { _ = s.srv.Serve(ln) }()
	go s.monitor()

	e.mu.Lock()
	e.sessions = append(e.sessions, s)
	e.mu.Unlock()

	return fmt.Sprintf("http://127.0.0.1:%d/stream/%s", s.port, s.token), s.token[:8], nil
}

// serverOrigin extracts scheme://host from the paired server URL.
func serverOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// guard wraps every request with the origin allowlist. Requests without an
// Origin header (same-origin tooling, curl) still need the path token, so
// they are not a meaningful bypass.
func (s *embeddedSession) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.origin != "" {
			if o := r.Header.Get("Origin"); o != "" && o != s.origin {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
		}
		// CORS + Private Network Access for the paired page.
		if s.origin != "" {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", s.origin)
			h.Set("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Content-Type")
			h.Set("Access-Control-Max-Age", "600")
			if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
				h.Set("Access-Control-Allow-Private-Network", "true")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveStream pipes the feeder's fMP4 output to the page's media element.
func (s *embeddedSession) serveStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/stream/"+s.token {
		http.NotFound(w, r)
		return
	}
	f, err := s.swapFeeder(s.start)
	if err != nil {
		http.Error(w, "feeder failed: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer func() {
		s.mu.Lock()
		current := s.feeder == f
		s.mu.Unlock()
		if current {
			f.close()
		}
	}()

	s.noteClient(1)
	defer s.noteClient(-1)

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Set("X-Starhold-Mode", "companion-embedded")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 64<<10)
	idle := 0
	for {
		n, rerr := f.stdout.Read(buf)
		if n > 0 {
			idle = 0
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // reader disconnects get a grace period to reconnect
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			s.finishFeeder(f, rerr)
			if s.stopped() {
				return
			}
			s.mu.Lock()
			target := s.start
			s.mu.Unlock()
			next, err := s.swapFeeder(target)
			if err != nil {
				return
			}
			f = next
			continue
		}
		if n == 0 {
			// no bytes right now: paused, buffering, or stalled
			s.mu.Lock()
			paused := s.paused
			s.mu.Unlock()
			if paused {
				idle = 0 // pausing legitimately stalls the pipe
				time.Sleep(embeddedIdleWait)
				continue
			}
			idle++
			if time.Duration(idle)*embeddedIdleWait > embeddedOrphanTimeout {
				go s.stop()
				return
			}
			time.Sleep(embeddedIdleWait)
		}
	}
}

// swapFeeder kills any existing feeder and spawns a fresh encode-mode mpv
// starting at from. The source URL travels only via IPC loadfile; the device
// token travels only via an IPC-configured header — neither is in argv.
func (s *embeddedSession) swapFeeder(from float64) (*feeder, error) {
	s.feederMu.Lock()
	defer s.feederMu.Unlock()
	if s.stopped() {
		return nil, errors.New("session stopped")
	}
	s.mu.Lock()
	old := s.feeder
	s.feeder = nil
	s.sock = ""
	s.mu.Unlock()
	if old != nil {
		old.close()
	}

	f := &feeder{}
	dir := os.TempDir()
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	f.sock = dir + "/wyrmhoard-feed-" + hex.EncodeToString(b) + ".sock"

	args := []string{
		// Encode mode: nothing touches real devices — decode feeds the
		// encoder, so neither output is used. Do NOT pass --vo/--ao here:
		// mpv remaps them to the encoder internally, and overriding them
		// (e.g. --ao=null) breaks encode-mode audio init entirely — the
		// feeder then stalls ~25s and dies with audio=eof/video=eof.
		"--no-terminal", "--force-window=no",
		// ytdl_hook would fire an extra unauthenticated fetch of the source
		// URL and slow the open by seconds for no benefit.
		"--ytdl=no",
		"--idle=yes", "--no-input-default-bindings",
		"--input-ipc-server=" + f.sock,
		"--user-agent=Starhold-Companion/" + version,
		"--log-file=" + f.sock + ".log",
	}
	if s.audioOnly {
		args = append(args, "--no-video")
	}
	if from > 0 {
		args = append(args, fmt.Sprintf("--start=%.2f", from))
	}
	args = append(args,
		"--o=-", "--of=mp4",
		"--ovc=libx264", "--oac=aac",
		"--ovcopts-add=preset=veryfast", "--ovcopts-add=crf=23",
		"--ovcopts-add=maxrate=8M", "--ovcopts-add=bufsize=16M",
		// Playback path, not archival: halve the encode cost (720p) and
		// disable x264 lookahead so the first frames ship immediately.
		"--vf-add=format=yuv420p,scale=-2:720",
		"--ovcopts-add=tune=zerolatency",
		"--oacopts-add=b=192k",
		// Fragmented MP4 must be set on the MUXER (of), not the video
		// encoder — this is what makes the mp4 muxer accept a pipe.
		"--ofopts-add=movflags=+frag_keyframe+empty_moov+default_base_moof",
	)
	f.cmd = exec.Command("mpv", args...)
	prepareProcess(f.cmd)
	var errbuf bytes.Buffer
	f.err = &errbuf
	f.cmd.Stderr = &errbuf
	stdout, err := f.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	f.stdout = stdout
	if err := f.cmd.Start(); err != nil {
		return nil, fmt.Errorf("feeder start: %w", err)
	}
	if err := privateFile(f.sock + ".log"); err != nil {
		f.close()
		return nil, errors.New("could not secure feeder log")
	}
	if err := waitSocket(f.sock, 5*time.Second); err != nil {
		f.close()
		return nil, f.failErr("feeder IPC did not start")
	}
	// mpv creates the socket file slightly before it starts serving it; a
	// naive first write lands in that window and dies with EPIPE (observed as
	// "Write error (Broken pipe)" and a feeder that never loads). Retry the
	// handshake until mpv answers.
	var ipcerr error
	for i := 0; i < 25; i++ {
		ipcerr = ipc(f.sock, []any{"set_property", "http-header-fields", []string{"Authorization: Bearer " + s.srcToken}})
		if ipcerr == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if ipcerr != nil {
		f.close()
		return nil, f.failErr("could not configure feeder headers")
	}
	if err := ipc(f.sock, []any{"loadfile", s.srcURL, "replace"}); err != nil {
		f.close()
		return nil, f.failErr("could not load source in feeder")
	}
	if s.stopped() {
		f.close()
		return nil, errors.New("session stopped")
	}
	s.mu.Lock()
	s.feeder = f
	s.sock = f.sock
	paused := s.paused
	s.mu.Unlock()
	if paused {
		_ = ipc(f.sock, []any{"set_property", "pause", true})
	}
	return f, nil
}

// serveControl handles page-driven transport over the session token.
// Body: {command: "pause"|"play"|"seek"|"position"|"stop", value: seconds}.
// A seek kills the feeder; the page reconnects its media element (restart).
func (s *embeddedSession) serveControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Command string  `json:"command"`
		Value   float64 `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	restart := false
	switch req.Command {
	case "pause":
		s.mu.Lock()
		s.paused = true
		sock := s.sock
		s.mu.Unlock()
		_ = ipc(sock, []any{"set_property", "pause", true})
	case "play":
		s.mu.Lock()
		s.paused = false
		sock := s.sock
		s.mu.Unlock()
		_ = ipc(sock, []any{"set_property", "pause", false})
	case "seek":
		if req.Value < 0 || req.Value > 24*3600 {
			http.Error(w, "bad value", http.StatusBadRequest)
			return
		}
		s.seek(req.Value)
		restart = true
	case "position":
		// read-only poll; state below comes from the monitor
	case "stop":
		s.stop()
		writeControlState(w, s, false)
		return
	default:
		http.Error(w, "unknown command", http.StatusBadRequest)
		return
	}
	writeControlState(w, s, restart)
}

func (s *embeddedSession) seek(seconds float64) {
	s.feederMu.Lock()
	defer s.feederMu.Unlock()
	s.mu.Lock()
	s.start = seconds
	s.ended = false
	f := s.feeder
	s.feeder = nil
	s.sock = ""
	s.mu.Unlock()
	if f != nil {
		f.close()
	}
}

func writeControlState(w http.ResponseWriter, s *embeddedSession, restart bool) {
	s.mu.Lock()
	st := map[string]any{"position": s.position, "duration": s.duration, "paused": s.paused, "ended": s.ended}
	s.mu.Unlock()
	if restart {
		st["restart"] = true
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "playback": st})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// monitor refreshes position/duration from the feeder, posts progress to the
// server, and tears the session down when nobody needs it.
func (s *embeddedSession) monitor() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	everClient := false
	tick := 0
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		tick++
		s.mu.Lock()
		sock := s.sock
		s.mu.Unlock()
		if sock != "" {
			if pos, err := ipcGet(sock, "time-pos"); err == nil && pos > 0 {
				s.mu.Lock()
				s.position = pos
				s.mu.Unlock()
			}
			if dur, err := ipcGet(sock, "duration"); err == nil && dur > 0 {
				s.mu.Lock()
				s.duration = dur
				s.mu.Unlock()
			}
		}
		s.mu.Lock()
		ended := s.ended
		clients := s.clients
		disconnectedAt := s.disconnectedAt
		if clients > 0 {
			everClient = true
		}
		s.mu.Unlock()

		if ended {
			s.reportProgress("stopped")
			s.stop()
			return
		}
		if everClient && clients == 0 && !disconnectedAt.IsZero() && time.Since(disconnectedAt) > embeddedIdleTimeout {
			s.stop()
			return
		}
		if !everClient && time.Since(s.born) > embeddedOrphanTimeout {
			s.stop() // page never attached at all
			return
		}
		if clients > 0 && s.itemID != "" && tick%embeddedProgressEvery == 0 {
			s.reportProgress("playing")
		}
	}
}

// reportProgress posts the current position to the paired server.
func (s *embeddedSession) reportProgress(state string) {
	s.mu.Lock()
	pos := s.position
	s.mu.Unlock()
	body, _ := json.Marshal(map[string]any{
		"item_id": s.itemID, "position_seconds": pos, "state": state,
	})
	req, err := http.NewRequest("POST", s.server+"/api/v1/companion/playback", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.srcToken)
	if resp, err := companionClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// stop tears down the feeder and the listener exactly once.
func (s *embeddedSession) stop() {
	s.stopOnce.Do(func() {
		close(s.done)
		s.feederMu.Lock()
		s.mu.Lock()
		f := s.feeder
		s.feeder = nil
		s.sock = ""
		s.mu.Unlock()
		s.feederMu.Unlock()
		if f != nil {
			f.close()
		}
		if s.srv != nil {
			_ = s.srv.Close()
		}
		if s.ln != nil {
			_ = s.ln.Close()
		}
	})
}

// playEmbedded handles {type:"play", embedded:true}: resolves the
// device-authorized stream from the server exactly like windowed play, then
// starts the loopback relay and returns its URL for the page's media element.
func playEmbedded(req request) response {
	c := loadConfig()
	if c.Token == "" {
		return response{ID: req.ID, OK: false, Error: "companion is not paired"}
	}
	itemID, channelID := req.ItemID, req.ChannelID
	if itemID == "" && channelID == "" {
		return response{ID: req.ID, OK: false, Error: "item_id or channel_id required"}
	}
	play := map[string]any{"position_seconds": req.Start, "state": "playing"}
	if channelID != "" {
		play["channel_id"] = channelID
	} else {
		play["item_id"] = itemID
	}
	body, _ := json.Marshal(play)
	httpReq, _ := http.NewRequest("POST", c.Server+"/api/v1/companion/playback", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := companionClient.Do(httpReq)
	if err != nil {
		return response{ID: req.ID, OK: false, Error: "cannot reach server"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return response{ID: req.ID, OK: false, Error: fmt.Sprintf("server rejected playback (%d)", resp.StatusCode)}
	}
	var p struct {
		URL string `json:"stream_url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&p)
	if p.URL == "" {
		return response{ID: req.ID, OK: false, Error: "no stream URL"}
	}
	stream := p.URL
	if strings.HasPrefix(stream, "/") {
		stream = c.Server + stream
	}
	if !sameServer(c.Server, stream) {
		return response{ID: req.ID, OK: false, Error: "server returned an invalid stream URL"}
	}
	// one active playback at a time: windowed loses to embedded
	player.stop()
	url, sid, err := startEmbedded(c, itemID, channelID, stream, req.Title, req.Start, req.AudioOnly)
	if err != nil {
		return response{ID: req.ID, OK: false, Error: err.Error()}
	}
	return response{ID: req.ID, OK: true, State: "playing", StreamURL: url, SessionID: sid, Paired: true, Server: c.Server}
}

// sessionByID resolves a live session by its public 8-hex id (token prefix).
func sessionByID(id string) *embeddedSession {
	if id == "" {
		return nil
	}
	embeddedPlayers.mu.Lock()
	defer embeddedPlayers.mu.Unlock()
	for _, s := range embeddedPlayers.sessions {
		select {
		case <-s.done:
			continue
		default:
		}
		if s.token == id || (len(id) == 8 && s.token[:8] == id) {
			return s
		}
	}
	return nil
}

func embeddedStatus(req request) response {
	s := sessionByID(req.SessionID)
	if s == nil {
		return response{ID: req.ID, OK: true, State: "idle"}
	}
	s.mu.Lock()
	st := &playbackState{Position: s.position, Duration: s.duration, Paused: s.paused, Ended: s.ended}
	s.mu.Unlock()
	return response{ID: req.ID, OK: true, State: "playing", Playback: st, SessionID: req.SessionID}
}

func embeddedStop(req request) response {
	s := sessionByID(req.SessionID)
	if s == nil {
		return response{ID: req.ID, OK: true, State: "idle"}
	}
	s.stop()
	return response{ID: req.ID, OK: true, State: "stopped"}
}

// embeddedControl handles transport commands for an embedded session over the
// native-messaging channel (used when the page talks through the extension
// rather than the loopback control endpoint).
func embeddedControl(req request) response {
	s := sessionByID(req.SessionID)
	if s == nil {
		return response{ID: req.ID, OK: false, Error: "no active embedded session"}
	}
	switch req.Command {
	case "pause", "play":
		paused := req.Command == "pause"
		s.mu.Lock()
		s.paused = paused
		sock := s.sock
		s.mu.Unlock()
		_ = ipc(sock, []any{"set_property", "pause", paused})
	case "stop":
		s.stop()
		return response{ID: req.ID, OK: true, State: "stopped"}
	case "seek":
		seconds, ok := toFloat(req.Value)
		if !ok || seconds < 0 || seconds > 24*3600 {
			return response{ID: req.ID, OK: false, Error: "invalid seek position"}
		}
		s.seek(seconds)
	case "position":
		// read-only poll
	default:
		return response{ID: req.ID, OK: false, Error: "unknown command"}
	}
	s.mu.Lock()
	st := &playbackState{Position: s.position, Duration: s.duration, Paused: s.paused, Ended: s.ended}
	s.mu.Unlock()
	return response{ID: req.ID, OK: true, Playback: st, SessionID: req.SessionID}
}
