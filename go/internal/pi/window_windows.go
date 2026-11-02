//go:build windows

package pi

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps a console window from flashing for every child process.
//
// A Go binary built with -H windowsgui has no console of its own, but a child
// inherits whatever the parent has; without this flag Pi's node process opens a
// window on the user's desktop for the duration of a turn.
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
