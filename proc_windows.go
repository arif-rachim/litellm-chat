package main

import (
	"os/exec"
	"strconv"
)

// killTree makes cancelling cmd kill its whole process tree: Windows has no
// process groups to signal, taskkill /T walks the children instead.
func killTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
