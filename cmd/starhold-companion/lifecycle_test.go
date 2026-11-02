package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeFeeder struct {
	closed bool
	read   io.ReadCloser
}

func (f *fakeFeeder) close() { f.closed = true }

func TestStreamRejectsWrongTokenWithoutStartingFeeder(t *testing.T) {
	s := &embeddedSession{token: "right-token", done: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/"+s.token, s.serveStream)
	got := httptest.NewRecorder()
	mux.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/stream/wrong-token", nil))
	if got.Code != http.StatusNotFound {
		t.Fatalf("wrong token should 404, got %d", got.Code)
	}
}

func TestStaleFeederCompletionCannotEndReplacement(t *testing.T) {
	s := &embeddedSession{token: "abc", done: make(chan struct{})}
	old := &fakeFeeder{}
	s.feeder = old
	s.finishFeeder(old, io.EOF)
	if !s.ended {
		t.Fatal("current feeder EOF should mark playback ended")
	}
	s.ended = false
	current := &fakeFeeder{}
	s.feeder = current
	s.finishFeeder(old, io.EOF)
	if s.ended || current.closed {
		t.Fatal("old feeder EOF must not stop replacement")
	}
	s.finishFeeder(current, io.EOF)
	if !s.ended {
		t.Fatal("current feeder EOF should mark playback ended")
	}
}

func TestDisconnectedClientGetsReconnectGrace(t *testing.T) {
	s := &embeddedSession{token: "abc", done: make(chan struct{})}
	s.noteClient(1)
	s.noteClient(-1)
	select {
	case <-s.done:
		t.Fatal("disconnect must not stop session immediately")
	default:
	}
	if s.disconnectedAt.IsZero() {
		t.Fatal("disconnect time should be recorded")
	}
	s.noteClient(1)
	if !s.disconnectedAt.IsZero() {
		t.Fatal("reconnect should clear disconnect deadline")
	}
}

func TestSessionRequiresFullToken(t *testing.T) {
	s := &embeddedSession{token: "deadbeef0000", done: make(chan struct{})}
	embeddedPlayers.sessions = []*embeddedSession{s}
	defer func() { embeddedPlayers.sessions = nil }()
	if sessionByID("dead") != nil {
		t.Fatal("short token prefix must not resolve")
	}
	if sessionByID("deadbeef0000") != s {
		t.Fatal("full session token should resolve")
	}
	if sessionByID("deadbeef") != s {
		t.Fatal("public id should resolve only as the exact prefix")
	}
}

func TestNativeSeekUsesRestartProtocol(t *testing.T) {
	s := &embeddedSession{token: "deadbeef0000", done: make(chan struct{})}
	embeddedPlayers.sessions = []*embeddedSession{s}
	defer func() { embeddedPlayers.sessions = nil }()
	r := embeddedControl(request{SessionID: "deadbeef", Command: "seek", Value: float64(42)})
	if !r.OK {
		t.Fatalf("native seek failed: %s", r.Error)
	}
	if s.start != 42 {
		t.Fatalf("seek target = %v, want 42", s.start)
	}
}

func TestStopAllEmbeddedSessions(t *testing.T) {
	a := &embeddedSession{done: make(chan struct{})}
	b := &embeddedSession{done: make(chan struct{})}
	embeddedPlayers.sessions = []*embeddedSession{a, b}
	defer func() { embeddedPlayers.sessions = nil }()
	stopAllEmbedded()
	for _, s := range []*embeddedSession{a, b} {
		select {
		case <-s.done:
		default:
			t.Fatal("session was not stopped")
		}
	}
}

func TestBoundedHTTPClientRejectsRedirects(t *testing.T) {
	srv := httptest.NewServer(http.RedirectHandler("https://example.com", http.StatusFound))
	defer srv.Close()
	_, err := companionClient.Get(srv.URL)
	if err == nil {
		t.Fatal("redirect must be rejected")
	}
	if companionClient.Timeout <= 0 || companionClient.Timeout > 30*time.Second {
		t.Fatalf("unexpected client timeout: %v", companionClient.Timeout)
	}
}

func TestFeederLogsArePrivate(t *testing.T) {
	path := t.TempDir() + "/feed.log"
	if err := privateFile(path); err != nil {
		t.Fatal(err)
	}
	st, err := pathStat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st != 0o600 {
		t.Fatalf("log mode = %o, want 600", st)
	}
}

func TestRedactsAuthorizationAndStreamTokens(t *testing.T) {
	msg := `http-header-fields: Authorization: Bearer TOPSECRET, http://127.0.0.1:1234/stream/abcdef1234567890abcdef1234567890abcdef1234567890`
	got := redactLog(msg)
	if strings.Contains(got, "TOPSECRET") || strings.Contains(got, "abcdef1234567890") {
		t.Fatalf("sensitive log content retained: %s", got)
	}
}
