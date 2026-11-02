package main

// subtitle_probe.go resolves an ffmpeg subtitle stream index to the track id
// mpv uses for --sid. Matroska track ids are not the same as ffmpeg's global
// stream indexes, so guessing the ordinal gets the wrong language on files
// with several audio/subtitle tracks. A short headless mpv probe reads only
// the container headers, then the real feeder starts with the correct --sid
// and burns subtitles from the first frame.

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"
)

func ipcProperty(sock, prop string) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	b, _ := json.Marshal(map[string]any{"command": []any{"get_property", prop}})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	var rep struct {
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	if err := json.NewDecoder(conn).Decode(&rep); err != nil {
		return nil, err
	}
	if rep.Error != "" && rep.Error != "success" {
		return nil, errors.New("mpv: " + rep.Error)
	}
	return rep.Data, nil
}

// resolveSubtitleSID returns mpv's track id for the given ffmpeg subtitle
// stream index, or 0 when it cannot be resolved.
func resolveSubtitleSID(srcURL, token string, ffIndex int) int {
	if ffIndex <= 0 {
		return 0
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	sock := os.TempDir() + "/starhold-probe-" + fmt.Sprintf("%x", b) + ".sock"
	args := []string{
		"--no-terminal", "--force-window=no", "--no-config", "--ytdl=no",
		"--idle=yes", "--no-input-default-bindings",
		"--vo=null", "--ao=null",
		"--input-ipc-server=" + sock,
		"--user-agent=Starhold-Companion/" + version,
	}
	cmd := exec.Command("mpv", args...)
	prepareProcess(cmd)
	if err := cmd.Start(); err != nil {
		return 0
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(sock)
	}()
	if err := waitSocket(sock, 5*time.Second); err != nil {
		return 0
	}
	for i := 0; i < 25; i++ {
		if err := ipc(sock, []any{"set_property", "http-header-fields", []string{"Authorization: Bearer " + token}}); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := ipc(sock, []any{"loadfile", srcURL, "replace"}); err != nil {
		return 0
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := ipcProperty(sock, "track-list")
		if err == nil {
			var tracks []struct {
				ID      int    `json:"id"`
				Type    string `json:"type"`
				FFIndex int    `json:"ff-index"`
			}
			if json.Unmarshal(raw, &tracks) == nil && len(tracks) > 0 {
				for _, t := range tracks {
					if t.Type == "sub" && t.FFIndex == ffIndex {
						return t.ID
					}
				}
				return 0 // tracks are loaded; this index is not a subtitle
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return 0
}
