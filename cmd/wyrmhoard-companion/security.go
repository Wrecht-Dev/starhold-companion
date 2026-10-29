package main

import (
	"errors"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"
)

var companionClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return errors.New("redirect not allowed")
	},
}

var bearerLogPattern = regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)[^\s,]+`)
var sessionLogPattern = regexp.MustCompile(`(/(?:stream|control)/)[0-9a-f]{16,}`)

func redactLog(msg string) string {
	msg = bearerLogPattern.ReplaceAllString(msg, `${1}[redacted]`)
	return sessionLogPattern.ReplaceAllString(msg, `${1}[redacted]`)
}

func privateFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func pathStat(path string) (os.FileMode, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Mode().Perm(), nil
}

var playbackLifecycle sync.RWMutex

func stopAllEmbedded() {
	embeddedPlayers.mu.Lock()
	sessions := append([]*embeddedSession(nil), embeddedPlayers.sessions...)
	embeddedPlayers.sessions = nil
	embeddedPlayers.mu.Unlock()
	for _, s := range sessions {
		s.stop()
	}
}

func cleanupFeederArtifacts() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, entry := range entries {
		name := entry.Name()
		if len(name) < 16 || name[:16] != "wyrmhoard-feed-" {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(os.TempDir() + "/" + name)
		}
	}
}
