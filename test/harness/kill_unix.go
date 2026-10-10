//go:build !windows

package harness

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// killTree kills the process and its children. Process.Kill reaches only
// Syncthing's launcher; the worker child would keep the ports. The children
// are looked up rather than put in their own process group, so an
// interactive Ctrl-C still reaches them.
func killTree(cmd *exec.Cmd) {
	out, _ := exec.Command("pgrep", "-P", strconv.Itoa(cmd.Process.Pid)).Output()
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	_ = cmd.Process.Kill()
}
