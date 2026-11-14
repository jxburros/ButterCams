//go:build linux

package winsvc

import (
	"os/exec"
	"syscall"
)

// prepareChild asks the kernel to kill the child when the guardian dies, the
// closest Linux equivalent of a kill-on-close Job Object.
func prepareChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
