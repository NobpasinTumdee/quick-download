//go:build !windows

package downloader

import (
	"os/exec"
	"syscall"
)

func isWindows() bool { return false }

// configureProcAttr gives yt-dlp its own process group so we can signal the
// whole tree (yt-dlp plus the ffmpeg it spawns) at once.
func configureProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree signals the entire process group.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// A negative pid addresses the process group created by Setpgid.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
