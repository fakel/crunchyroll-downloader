//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group, so cancelling a
// download also stops the ffmpeg it started instead of leaving an orphan
// muxing into a directory nobody is watching.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup asks the whole group to stop. SIGTERM rather than SIGKILL so
// ffmpeg can remove its partial output.
func killProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGTERM)
}
