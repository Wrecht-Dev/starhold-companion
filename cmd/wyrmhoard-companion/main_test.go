package main

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSameServerRejectsPrefixLookalikes(t *testing.T) {
	cases := []struct {
		base, target string
		ok           bool
	}{
		{"https://media.example.com", "https://media.example.com/api/v1/companion/live/x", true},
		{"https://media.example.com/base", "https://media.example.com/base/api", true},
		{"https://media.example.com", "https://media.example.com.evil/api", false},
		{"https://media.example.com", "http://media.example.com/api", false},
		{"https://media.example.com", "https://evil.example/api", false},
		{"https://media.example.com", "https://media.example.com@evil/api", false},
	}
	for _, c := range cases {
		if got := sameServer(c.base, c.target); got != c.ok {
			t.Errorf("sameServer(%q,%q)=%v, want %v", c.base, c.target, got, c.ok)
		}
	}
}

func TestConfigIsPrivate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	c := config{Server: "https://media.example", Token: "secret", Device: "d", Name: "n"}
	if err := saveConfig(c); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "wyrmhoard", "companion.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestNativeMessagingFraming(t *testing.T) {
	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	writeMessage(w, response{OK: true, Version: version})
	if out.Len() < 4 {
		t.Fatal("missing length prefix")
	}
	r := bufio.NewReader(&out)
	msg, err := readMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(msg, []byte(`"ok":true`)) {
		t.Fatalf("bad message %s", msg)
	}
	if _, err := readMessage(r); err == nil {
		t.Fatal("expected EOF after one framing")
	}
}

func TestPlayDoesNotPutTokenInMpvArgv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MPV_ARGV_LOG", filepath.Join(dir, "argv.log"))
	t.Setenv("MPV_CMD_LOG", filepath.Join(dir, "cmd.log"))
	script := `#!/usr/bin/env python3
import os, socket, sys
sock = next(a.split("=", 1)[1] for a in sys.argv if a.startswith("--input-ipc-server="))
with open(os.environ["MPV_ARGV_LOG"], "w") as f:
    f.write("\n".join(sys.argv))
s = socket.socket(socket.AF_UNIX)
s.bind(sock)
s.listen(8)
while True:
    c, _ = s.accept()
    line = c.makefile("rb").readline()
    if line:
        with open(os.environ["MPV_CMD_LOG"], "a") as f:
            f.write(line.decode())
        try:
            c.sendall(b'{"error":"success","data":null}\n')
        except BrokenPipeError:
            pass
    c.close()
`
	fakeDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(fakeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mpvPath := filepath.Join(fakeDir, "mpv")
	if err := os.WriteFile(mpvPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+":/usr/bin:/bin")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stream_url":"/api/v1/companion/stream/abc"}`))
	}))
	defer srv.Close()
	if err := saveConfig(config{Server: srv.URL, Token: "TOPSECRET", Device: "d1", Name: "test"}); err != nil {
		t.Fatal(err)
	}
	r := handle(request{Type: "play", ItemID: "abc", Title: "test", Start: 3})
	if !r.OK {
		t.Fatalf("play failed: %s", r.Error)
	}
	defer player.stop()
	time.Sleep(300 * time.Millisecond)
	argv, _ := os.ReadFile(os.Getenv("MPV_ARGV_LOG"))
	if bytes.Contains(argv, []byte("TOPSECRET")) || bytes.Contains(argv, []byte("companion/stream/abc")) {
		t.Fatalf("token/url leaked into mpv argv: %s", argv)
	}
	cmd, _ := os.ReadFile(os.Getenv("MPV_CMD_LOG"))
	if !bytes.Contains(cmd, []byte("http-header-fields")) || !bytes.Contains(cmd, []byte("TOPSECRET")) || !bytes.Contains(cmd, []byte("loadfile")) {
		t.Fatalf("IPC did not receive header/loadfile: %s", cmd)
	}
}
