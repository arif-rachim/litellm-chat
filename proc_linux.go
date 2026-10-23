package main

import (
	"os/exec"
	"syscall"
)

// killTree makes cancelling cmd kill its whole process group, so a test
// runner's children do not outlive a timeout.
func killTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
