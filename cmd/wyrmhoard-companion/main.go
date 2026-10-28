// wyrmhoard-companion is a small native-messaging host that plays the original
// file through an installed mpv process. It never transcodes and never exposes
// a local network port to web pages.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
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
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const version = "0.2.0"

type config struct {
	Server string `json:"server"`
	Token  string `json:"device_token"`
	Device string `json:"device_id"`
	Name   string `json:"device_name"`
}

type request struct {
	ID        any     `json:"id"`
	Type      string  `json:"type"`
	Server    string  `json:"server"`
	Code      string  `json:"code"`
	URL       string  `json:"url"`
	ItemID    string  `json:"item_id"`
	ChannelID string  `json:"channel_id"`
	SessionID string  `json:"session_id"`
	Command   string  `json:"command"`
	Value     any     `json:"value"`
	Start     float64 `json:"start"`
	Title     string  `json:"title"`
	// Embedded requests the in-page player path (headless mpv → loopback
	// fMP4 stream consumed by the web app's own <video> element; no window).
	Embedded  bool `json:"embedded,omitempty"`
	AudioOnly bool `json:"audio_only,omitempty"`
}

type response struct {
	ID       any     `json:"id,omitempty"`
	OK       bool    `json:"ok"`
	Error    string  `json:"error,omitempty"`
	Version  string  `json:"version,omitempty"`
	Paired   bool    `json:"paired,omitempty"`
	Server   string  `json:"server,omitempty"`
	Device   string  `json:"device,omitempty"`
	State    string  `json:"state,omitempty"`
	Position float64 `json:"position,omitempty"`
	Duration float64 `json:"duration,omitempty"`
	Mpv      bool    `json:"mpv,omitempty"`
	// Embedded playback
	StreamURL string         `json:"stream_url,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Playback  *playbackState `json:"playback,omitempty"`
}

// playbackState is the transport state of an embedded session.
type playbackState struct {
	Position float64 `json:"position"`
	Duration float64 `json:"duration"`
	Paused   bool    `json:"paused"`
	Ended    bool    `json:"ended"`
}

type mpv struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	socket  string
	title   string
	itemID  string
	server  string
	token   string
	stopped chan struct{}
}

var player = &mpv{}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(version)
		return
	}
	if runPairCLI() {
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "update" {
		if err := selfUpdate(); err != nil {
			fmt.Fprintln(os.Stderr, "update:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "update: complete (restart the browser to use it)")
		return
	}
	selfUpdateLoop()
	// Native messaging uses stdin/stdout; logs must go to stderr.
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	for {
		msg, err := readMessage(in)
		if err != nil {
			if err != io.EOF {
				fmt.Fprintln(os.Stderr, "read:", err)
			}
			return
		}
		var req request
		if err := json.Unmarshal(msg, &req); err != nil {
			writeMessage(out, response{Error: "invalid json", OK: false})
			continue
		}
		writeMessage(out, handle(req))
	}
}

func readMessage(r *bufio.Reader) ([]byte, error) {
	var size uint32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		return nil, err
	}
	if size > 1<<20 {
		return nil, errors.New("message too large")
	}
	buf := make([]byte, size)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

func writeMessage(w *bufio.Writer, v response) {
	b, _ := json.Marshal(v)
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(b)))
	_, _ = w.Write(size[:])
	_, _ = w.Write(b)
	_ = w.Flush()
}

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "wyrmhoard", "companion.json")
}

func loadConfig() config {
	var c config
	b, err := os.ReadFile(configPath())
	if err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(c config) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

func validateServer(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("invalid server URL")
	}
	// Never allow a file:, javascript: or credential-bearing URL.
	if u.User != nil || strings.ContainsAny(u.Host, "\r\n") {
		return "", errors.New("invalid server URL")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func handle(req request) response {
	switch req.Type {
	case "hello", "ping":
		c := loadConfig()
		return response{ID: req.ID, OK: true, Version: version, Paired: c.Token != "", Server: c.Server, Device: c.Device, Mpv: mpvAvailable()}
	case "pair":
		return pair(req)
	case "play":
		if req.Embedded {
			return playEmbedded(req)
		}
		return play(req)
	case "embedded_status":
		return embeddedStatus(req)
	case "embedded_stop":
		return embeddedStop(req)
	case "control":
		return control(req)
	case "status":
		return status(req)
	default:
		return response{ID: req.ID, OK: false, Error: "unknown command"}
	}
}

func pair(req request) response {
	server, err := validateServer(req.Server)
	if err != nil {
		return response{ID: req.ID, OK: false, Error: err.Error()}
	}
	body, _ := json.Marshal(map[string]string{
		"code": strings.TrimSpace(req.Code), "name": hostname(), "platform": runtime.GOOS + "/" + runtime.GOARCH, "version": version,
	})
	resp, err := http.Post(server+"/api/v1/companion/pair/complete", "application/json", bytes.NewReader(body))
	if err != nil {
		return response{ID: req.ID, OK: false, Error: "cannot reach server"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		return response{ID: req.ID, OK: false, Error: "pairing rejected (" + resp.Status + ")"}
	}
	var out struct {
		Token  string `json:"device_token"`
		Device struct {
			ID string `json:"id"`
		} `json:"device"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		return response{ID: req.ID, OK: false, Error: "invalid pairing response"}
	}
	c := config{Server: server, Token: out.Token, Device: out.Device.ID, Name: hostname()}
	if err := saveConfig(c); err != nil {
		return response{ID: req.ID, OK: false, Error: "cannot save pairing"}
	}
	return response{ID: req.ID, OK: true, Paired: true, Server: server, Device: c.Device}
}

func play(req request) response {
	c := loadConfig()
	if c.Token == "" {
		return response{ID: req.ID, OK: false, Error: "companion is not paired"}
	}
	if !mpvAvailable() {
		return response{ID: req.ID, OK: false, Error: "mpv is not installed"}
	}
	itemID := req.ItemID
	start := req.Start
	if itemID == "" && req.ChannelID == "" {
		return response{ID: req.ID, OK: false, Error: "item_id or channel_id required"}
	}
	// The companion asks the server for a device-authorized stream. It never
	// opens arbitrary URLs or local paths.
	play := map[string]any{"position_seconds": start, "state": "playing"}
	if req.ChannelID != "" {
		play["channel_id"] = req.ChannelID
	} else {
		play["item_id"] = itemID
	}
	body, _ := json.Marshal(play)
	httpReq, _ := http.NewRequest("POST", c.Server+"/api/v1/companion/playback", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := http.DefaultClient.Do(httpReq)
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
	player.stop()
	if err := player.start(c, itemID, stream, req.Title, start); err != nil {
		return response{ID: req.ID, OK: false, Error: err.Error()}
	}
	return response{ID: req.ID, OK: true, State: "playing", Paired: true, Server: c.Server}
}

func control(req request) response {
	if req.SessionID != "" {
		return embeddedControl(req)
	}
	player.mu.Lock()
	cmd := player.cmd
	sock := player.socket
	player.mu.Unlock()
	if cmd == nil {
		return response{ID: req.ID, OK: false, Error: "nothing is playing"}
	}
	switch req.Command {
	case "pause", "play":
		paused := req.Command == "pause"
		_ = ipc(sock, []any{"set_property", "pause", paused})
	case "seek":
		sec, _ := toFloat(req.Value)
		_ = ipc(sock, []any{"seek", sec, "absolute"})
	case "volume":
		v, _ := toFloat(req.Value)
		_ = ipc(sock, []any{"set_property", "volume", v})
	case "audio":
		_ = ipc(sock, []any{"set_property", "aid", req.Value})
	case "subtitle":
		_ = ipc(sock, []any{"set_property", "sid", req.Value})
	case "stop":
		player.stop()
		return response{ID: req.ID, OK: true, State: "stopped"}
	default:
		return response{ID: req.ID, OK: false, Error: "unknown control"}
	}
	return response{ID: req.ID, OK: true, State: "ok"}
}

func status(req request) response {
	player.mu.Lock()
	cmd := player.cmd
	sock := player.socket
	player.mu.Unlock()
	if cmd == nil {
		return response{ID: req.ID, OK: true, State: "idle", Mpv: mpvAvailable()}
	}
	pos, _ := ipcGet(sock, "time-pos")
	dur, _ := ipcGet(sock, "duration")
	return response{ID: req.ID, OK: true, State: "playing", Position: pos, Duration: dur}
}

func mpvAvailable() bool {
	_, err := exec.LookPath("mpv")
	return err == nil
}

func (m *mpv) start(c config, itemID, stream, title string, start float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	dir := os.TempDir()
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	sock := filepath.Join(dir, "wyrmhoard-mpv-"+hex.EncodeToString(b)+".sock")
	// Launch mpv without the URL or device token in argv (both are visible in
	// the process list). Configure the header over the private IPC socket and
	// only then load the server-issued URL.
	args := []string{
		"--force-window=yes", "--keep-open=no", "--idle=yes", "--pause=yes",
		"--input-ipc-server=" + sock,
		"--user-agent=Starhold-Companion/" + version,
		"--title=" + title,
	}
	cmd := exec.Command("mpv", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := waitSocket(sock, 5*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return errors.New("mpv IPC did not start")
	}
	if err := ipc(sock, []any{"set_property", "http-header-fields", []string{"Authorization: Bearer " + c.Token}}); err != nil {
		_ = cmd.Process.Kill()
		return errors.New("could not configure mpv headers")
	}
	loadArgs := []any{"loadfile", stream, "replace"}
	if start > 0 {
		loadArgs = append(loadArgs, fmt.Sprintf("start=%.2f", start))
	}
	if err := ipc(sock, loadArgs); err != nil {
		_ = cmd.Process.Kill()
		return errors.New("could not start mpv playback")
	}
	_ = ipc(sock, []any{"set_property", "idle", "no"})
	_ = ipc(sock, []any{"set_property", "pause", false})
	m.cmd, m.socket, m.itemID, m.server, m.token, m.title = cmd, sock, itemID, c.Server, c.Token, title
	go m.wait()
	go m.progress()
	return nil
}

func (m *mpv) wait() {
	m.mu.Lock()
	cmd := m.cmd
	m.mu.Unlock()
	if cmd != nil {
		_ = cmd.Wait()
		m.mu.Lock()
		if m.cmd == cmd {
			m.cmd = nil
		}
		m.mu.Unlock()
	}
}

func (m *mpv) stop() {
	m.mu.Lock()
	cmd, sock := m.cmd, m.socket
	m.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = ipc(sock, []any{"quit"})
	time.Sleep(300 * time.Millisecond)
	_ = cmd.Process.Kill()
}

func (m *mpv) progress() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		cmd, sock, item, server, token := m.cmd, m.socket, m.itemID, m.server, m.token
		m.mu.Unlock()
		if cmd == nil {
			return
		}
		pos, _ := ipcGet(sock, "time-pos")
		body, _ := json.Marshal(map[string]any{"item_id": item, "position_seconds": pos, "state": "playing"})
		req, _ := http.NewRequest("POST", server+"/api/v1/companion/playback", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
}

func ipc(sock string, cmd []any) error {
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	b, _ := json.Marshal(map[string]any{"command": cmd})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return err
	}
	// Drain the reply so mpv never sees a broken pipe; surface mpv-level
	// errors ("success" vs anything else) instead of fire-and-forget.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var rep struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(conn).Decode(&rep); err != nil {
		return nil // no reply in time is not fatal; the command was delivered
	}
	if rep.Error != "" && rep.Error != "success" {
		return errors.New("mpv: " + rep.Error)
	}
	return nil
}

func ipcGet(sock, prop string) (float64, error) {
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	b, _ := json.Marshal(map[string]any{"command": []any{"get_property", prop}})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return 0, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var out struct {
		Data float64 `json:"data"`
	}
	if err := json.NewDecoder(conn).Decode(&out); err != nil {
		return 0, err
	}
	return out.Data, nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "Companion"
	}
	return h
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		var f float64
		_, err := fmt.Sscanf(x, "%f", &f)
		return f, err == nil
	}
	return 0, false
}

func waitSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("timeout")
}

func sameServer(base, target string) bool {
	b, err := url.Parse(base)
	if err != nil {
		return false
	}
	t, err := url.Parse(target)
	if err != nil || t.Scheme != b.Scheme || t.Host != b.Host {
		return false
	}
	bp := strings.TrimRight(b.Path, "/")
	return bp == "" || t.Path == bp || strings.HasPrefix(t.Path, bp+"/")
}
