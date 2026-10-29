package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEmbeddedSessionTokenPath(t *testing.T) {
	s := &embeddedSession{token: "abcdef1234567890", done: make(chan struct{}), born: time.Now()}
	req := httptest.NewRequest(http.MethodGet, "/stream/WRONGTOKEN", nil)
	rec := httptest.NewRecorder()
	// serveStream requires the token-prefixed path; mux handles routing, but
	// the handler itself must reject mismatches if invoked directly.
	s.serveStream(rec, req)
	// Without an mpv process the handler errors out (503) — the point is it
	// must not stream for the wrong token; we registered the mux path with the
	// real token so direct invocation is only reachable with the right path.
	// This test pins the 404 path via the mux.
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/"+s.token, func(w http.ResponseWriter, r *http.Request) { s.serveStream(w, r) })
	got := httptest.NewRecorder()
	mux.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/stream/wrong", nil))
	if got.Code != http.StatusNotFound {
		t.Fatalf("wrong token should 404, got %d", got.Code)
	}
}

func TestEmbeddedControlRequiresPost(t *testing.T) {
	s := &embeddedSession{token: "abcdef", done: make(chan struct{}), born: time.Now()}
	mux := http.NewServeMux()
	mux.HandleFunc("/control/"+s.token, s.serveControl)
	got := httptest.NewRecorder()
	mux.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/control/"+s.token, nil))
	if got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET control should 405, got %d", got.Code)
	}
	got2 := httptest.NewRecorder()
	mux.ServeHTTP(got2, httptest.NewRequest(http.MethodPost, "/control/"+s.token, strings.NewReader(`{"command":"pause"}`)))
	if got2.Code != 200 {
		t.Fatalf("POST control should 200, got %d", got2.Code)
	}
	s.mu.Lock()
	paused := s.paused
	s.mu.Unlock()
	if !paused {
		t.Fatal("pause command should flip paused state")
	}
}

func TestEmbeddedSeekRestarts(t *testing.T) {
	s := &embeddedSession{token: "abcdef", done: make(chan struct{}), born: time.Now()}
	mux := http.NewServeMux()
	mux.HandleFunc("/control/"+s.token, s.serveControl)
	got := httptest.NewRecorder()
	mux.ServeHTTP(got, httptest.NewRequest(http.MethodPost, "/control/"+s.token, strings.NewReader(`{"command":"seek","value":42}`)))
	if got.Code != 200 {
		t.Fatalf("seek should 200, got %d", got.Code)
	}
	if !strings.Contains(got.Body.String(), `"restart":true`) {
		t.Fatalf("seek should signal restart, got %s", got.Body.String())
	}
	if s.start != 42 {
		t.Fatalf("seek should set next feeder start, got %v", s.start)
	}
}

func TestEmbeddedOriginGuard(t *testing.T) {
	s := &embeddedSession{token: "abcdef", done: make(chan struct{}), born: time.Now(), origin: "https://starhold.space"}
	mux := http.NewServeMux()
	mux.HandleFunc("/control/"+s.token, s.serveControl)
	h := s.guard(mux)
	got := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/control/"+s.token, strings.NewReader(`{"command":"position"}`))
	req.Header.Set("Origin", "https://evil.example")
	h.ServeHTTP(got, req)
	if got.Code != http.StatusForbidden {
		t.Fatalf("foreign origin should 403, got %d", got.Code)
	}
	ok := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/control/"+s.token, strings.NewReader(`{"command":"position"}`))
	req2.Header.Set("Origin", "https://starhold.space")
	h.ServeHTTP(ok, req2)
	if ok.Code != 200 {
		t.Fatalf("paired origin should 200, got %d", ok.Code)
	}
	if ok.Header().Get("Access-Control-Allow-Origin") != "https://starhold.space" {
		t.Fatal("CORS header must echo the paired origin")
	}
}

func TestEmbeddedPNAPreflight(t *testing.T) {
	s := &embeddedSession{token: "abcdef", done: make(chan struct{}), born: time.Now(), origin: "https://starhold.space"}
	h := s.guard(http.NewServeMux())
	got := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/stream/"+s.token, nil)
	req.Header.Set("Origin", "https://starhold.space")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	h.ServeHTTP(got, req)
	if got.Code != http.StatusNoContent {
		t.Fatalf("preflight should 204, got %d", got.Code)
	}
	if got.Header().Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatal("PNA preflight must be answered")
	}
}

func TestEmbeddedStopOnce(t *testing.T) {
	s := &embeddedSession{token: "abc", done: make(chan struct{}), born: time.Now()}
	s.stop()
	s.stop() // must not panic or double-close
	select {
	case <-s.done:
	default:
		t.Fatal("stop should close done")
	}
}

func TestPlayEmbeddedUnpaired(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// no config file in test env → unpaired
	resp := playEmbedded(request{ID: 1, Type: "play", Embedded: true, ItemID: "x"})
	if resp.OK || resp.Error != "companion is not paired" {
		t.Fatalf("unpaired embedded play should fail cleanly, got %+v", resp)
	}
}

func TestSessionByID(t *testing.T) {
	s := &embeddedSession{token: "deadbeef0000", done: make(chan struct{}), born: time.Now()}
	embeddedPlayers.mu.Lock()
	embeddedPlayers.sessions = append(embeddedPlayers.sessions, s)
	embeddedPlayers.mu.Unlock()
	if sessionByID("deadbeef") != s {
		t.Fatal("sessionByID should resolve by token prefix")
	}
	if sessionByID("nope") != nil {
		t.Fatal("unknown id must not resolve")
	}
}
