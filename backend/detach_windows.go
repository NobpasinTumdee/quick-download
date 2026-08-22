//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// Windows process creation flags (winbase.h).
const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// detach makes the engine survive the death of this host process and keeps it
// from flashing a console window on screen.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup,
		HideWindow:    true,
	}
}
