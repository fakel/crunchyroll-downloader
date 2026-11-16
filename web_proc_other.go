//go:build !unix

package main

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op where process groups don't exist; the release
// builds for Windows use this file.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup stops the child directly.
func killProcessGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
