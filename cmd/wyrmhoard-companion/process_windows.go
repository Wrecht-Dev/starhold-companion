//go:build windows

package main

import "os/exec"

func prepareProcess(_ *exec.Cmd) {}

func terminateProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
