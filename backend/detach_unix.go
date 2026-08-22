//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach puts the engine in its own session so it is not killed together with
// the short-lived native messaging host process.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
