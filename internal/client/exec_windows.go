//go:build windows

package client

import "os/exec"

// configureProcessGroup is a no-op on Windows: os/exec already terminates the
// child through the platform's job/process semantics, and SysProcAttr has no
// process-group knob here.
func configureProcessGroup(cmd *exec.Cmd) {}
