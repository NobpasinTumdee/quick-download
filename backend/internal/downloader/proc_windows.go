//go:build windows

package downloader

import (
	"os/exec"
	"strconv"
	"syscall"
)

const createNewProcessGroup = 0x00000200

func isWindows() bool { return true }

// configureProcAttr puts yt-dlp in its own process group and hides its console
// window (the daemon itself runs detached, so a console would flash on screen).
func configureProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup,
		HideWindow:    true,
	}
}

// killProcessTree terminates yt-dlp AND the ffmpeg it spawned.
//
// cmd.Process.Kill() only kills yt-dlp itself: ffmpeg would keep running and
// hold the output file open. taskkill /T walks the child tree.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	kill := exec.Command("taskkill", "/F", "/T", "/PID", pid)
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := kill.Run(); err != nil {
		// Fall back to killing just the parent; better than nothing.
		return cmd.Process.Kill()
	}
	return nil
}
