//go:build !windows

package pi

import "os/exec"

// hideWindow is a no-op outside Windows, where a child does not open a window.
func hideWindow(cmd *exec.Cmd) {}
