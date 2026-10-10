//go:build windows

package harness

import (
	"os/exec"
	"strconv"
)

// killTree kills the process and its children. Process.Kill reaches only
// Syncthing's launcher; the worker child would keep the ports.
func killTree(cmd *exec.Cmd) {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	_ = cmd.Process.Kill()
}
