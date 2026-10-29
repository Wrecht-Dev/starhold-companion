package main

// control.go — the loopback control server that lets the Starhold WEB APP talk
// to the companion directly, with no browser extension involved.
//
// Safety model (mirrors embedded.go and must never regress):
//   - Binds 127.0.0.1 on a small fixed port range (default 8955, then
//     8956-8959; WYRMHOARD_COMPANION_PORT pins one). Never a wildcard.
//   - Every request must arrive with Host 127.0.0.1 / localhost / [::1] —
//     this kills DNS-rebinding pages that point a name at 127.0.0.1.
//   - Origin is checked: /pair only accepts when the Origin host matches the
//     server being paired (a drive-by page cannot pair you to a server other
//     than the one it is served from); after pairing, everything else only
//     accepts the PAIRED server's origin.
//   - Chrome's Private Network Access is honored on preflights.
//   - Bodies are size-capped; /play and /pair are rate-limited.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	controlBodyLimit = 8 << 10
	controlPorts     = "8955,8956,8957,8958,8959"
)

type controlServer struct {
	mu       sync.Mutex
	port     int
	srv      *http.Server
	attempts map[string][]time.Time // rate limit per remote+path
}

var ctlServer = &controlServer{attempts: map[string][]time.Time{}}

// startControlServer binds the loopback port range; the first free port wins.
// A failure to bind every candidate is non-fatal: native messaging keeps
// working, only the extension-free path disappears.
func startControlServer() {
	spec := os.Getenv("WYRMHOARD_COMPANION_PORT")
	ports := controlPorts
	if spec != "" {
		if _, err := strconv.Atoi(spec); err == nil {
			ports = spec
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", ctlServer.guard(false, servePing))
	mux.HandleFunc("/pair", ctlServer.guard(true, servePair))
	mux.HandleFunc("/play", ctlServer.guard(false, servePlay))
	mux.HandleFunc("/control", ctlServer.guard(false, serveControl))
	mux.HandleFunc("/state", ctlServer.guard(false, serveState))
	mux.HandleFunc("/stop", ctlServer.guard(false, serveStop))
	mux.HandleFunc("/unpair", ctlServer.guard(false, serveUnpair))
	for _, p := range strings.Split(ports, ",") {
		port, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		ctlServer.port = port
		ctlServer.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = ctlServer.srv.Serve(ln) }()
		return
	}
}

// host loops back? Reject anything whose Host header is not a loopback form —
// a DNS-rebinding page would present its own hostname here.
func loopbackHost(r *http.Request) bool {
	h := r.Host
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.HasSuffix(h, "]") {
		h = h[:i]
	}
	switch strings.ToLower(h) {
	case "127.0.0.1", "localhost", "[::1]", "::1":
		return true
	}
	return false
}

// pairedOrigin reports scheme://host of the configured server, or "".
func pairedOrigin() string {
	c := loadConfig()
	if c.Server == "" || c.Token == "" {
		return ""
	}
	return originOf(c.Server)
}

func originOf(raw string) string {
	if i := strings.Index(raw, "://"); i > 0 {
		rest := raw[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		return raw[:i] + "://" + rest
	}
	return raw
}

// guard wraps a handler with the host check, the origin policy and CORS. When
// pairMode is true and the companion is unpaired, the Origin must match the
// server being paired; otherwise only the paired server's origin is allowed.
func (cs *controlServer) guard(pairMode bool, next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		w.Header().Set("Access-Control-Max-Age", "600")
		if !loopbackHost(r) {
			http.Error(w, "loopback only", http.StatusForbidden)
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" {
			allowed := pairedOrigin()
			if allowed == "" {
				// Unpaired: /ping is safe to expose (version + paired flag
				// only) and /pair is the way pairing happens without the
				// extension; both reflect to the calling origin. Everything
				// else stays locked until paired.
				if r.URL.Path != "/ping" && !pairMode {
					http.Error(w, "companion is not paired", http.StatusForbidden)
					return
				}
				allowed = origin // /pair happens next from this same page
			}
			if origin != allowed && r.URL.Path != "/ping" {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			// Reflect ACAO on preflights that return early below.
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !cs.allow(r) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, controlBodyLimit)
		}
		next(w, r)
	}
}

// allow is a tiny per-remote rate limiter: 30 requests / 10s per path.
func (cs *controlServer) allow(r *http.Request) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	key := r.RemoteAddr + "|" + r.URL.Path
	now := time.Now()
	window := now.Add(-10 * time.Second)
	recent := cs.attempts[key][:0]
	for _, t := range cs.attempts[key] {
		if t.After(window) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= 30 {
		cs.attempts[key] = recent
		return false
	}
	cs.attempts[key] = append(recent, now)
	return true
}

func decodeBody(r *http.Request) (request, bool) {
	var req request
	if r.Method != http.MethodPost {
		return req, false
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, false
	}
	return req, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func servePing(w http.ResponseWriter, r *http.Request) {
	c := loadConfig()
	writeJSON(w, response{OK: true, Version: version, Paired: c.Token != "", Server: c.Server, Device: c.Device, Mpv: mpvAvailable()})
}

func servePair(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeBody(r)
	if !ok {
		writeJSON(w, response{OK: false, Error: "invalid body"})
		return
	}
	// The pairing page may only pair its own server: a drive-by site cannot
	// aim the companion at some other host.
	if origin := r.Header.Get("Origin"); origin != "" && originOf(req.Server) != origin {
		writeJSON(w, response{OK: false, Error: "origin does not match the pairing server"})
		return
	}
	writeJSON(w, pair(req))
}

func servePlay(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeBody(r)
	if !ok {
		writeJSON(w, response{OK: false, Error: "invalid body"})
		return
	}
	req.Type = "play"
	writeJSON(w, handle(req))
}

func serveControl(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeBody(r)
	if !ok {
		writeJSON(w, response{OK: false, Error: "invalid body"})
		return
	}
	req.Type = "control"
	writeJSON(w, control(req))
}

func serveState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, status(request{}))
}

func serveStop(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, embeddedStop(request{}))
}

// serveUnpair clears the stored device token so the companion reports itself
// unpaired immediately (the web UI's Unpair button calls this after revoking
// the device server-side).
func serveUnpair(w http.ResponseWriter, r *http.Request) {
	c := loadConfig()
	c.Token = ""
	c.Device = ""
	if err := saveConfig(c); err != nil {
		writeJSON(w, response{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, response{OK: true, Paired: false})
}
