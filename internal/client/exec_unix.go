//go:build !windows

package client

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the captured command in its own process group and
// makes context cancellation kill the whole group. exec.CommandContext's
// default Cancel kills only the direct child, so a daemonized grandchild would
// keep running (and keep writing) after velruna returned.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the group created by Setpgid.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
