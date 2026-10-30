package main

// serve.go — background mode used by the systemd user service. It exposes
// only the loopback control server; stdin is not a native-messaging channel
// here, so the process must not fall into the framing loop and exit at EOF.

import (
	"os"
	"os/signal"
	"syscall"
)

func serveMode() {
	selfUpdateLoop()
	cleanupFeederArtifacts()
	startControlServer()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	signal.Stop(sig)

	playbackLifecycle.Lock()
	stopAllEmbedded()
	player.stop()
	playbackLifecycle.Unlock()
	if ctlServer.srv != nil {
		_ = ctlServer.srv.Close()
	}
}
