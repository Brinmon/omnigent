// Command server-wrapper lets the existing Playwright harness drive a Go
// omnigent server without touching a single test.
//
// The harness spawns its server subprocess as:
//
//	<exe> -c "<patch>; from omnigent.cli import main; main()" server --host ... \
//	      --port ... --database-uri ... --artifact-location ... --agent ...
//
// and points OMNIGENT_COMPAT_SERVER_PYTHON (or ..._RUNNER_PYTHON) at <exe>.
// Tests/_helpers/compat.py also strips PYTHONPATH and runs from a neutral CWD
// in compat mode, so the wrapper is the only way in.
//
// This wrapper drops the "-c <patch>" / "-m <module>" prefix and execs the Go
// server with the remaining argv. It is a native binary on purpose: the
// harness CreateProcess'es it directly, so a shell/.cmd shim would fail with
// WinError 2.
package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	args := os.Args[1:]

	// Drop the inline-Python prefix the harness prepends. The patch only
	// shortens presence leave grace; the Go server owns that as its own flag.
	for len(args) >= 2 && (args[0] == "-c" || args[0] == "-m") {
		args = args[2:]
	}

	target := os.Getenv("GO_SERVER_BIN")
	if target == "" {
		fmt.Fprintln(os.Stderr, "server-wrapper: set GO_SERVER_BIN to the server binary to run")
		os.Exit(2)
	}

	// GO_SERVER_MODULE_ENTRY=1 runs the target as an interpreter over
	// "-m omnigent" (used to point the wrapper at the baseline Python CLI when
	// proving the knob itself works). Unset means target is a standalone binary
	// that takes the server argv directly — the Go server case.
	if os.Getenv("GO_SERVER_MODULE_ENTRY") == "1" {
		args = append([]string{"-m", "omnigent"}, args...)
	}

	cmd := exec.Command(target, args...)
	cmd.Env = os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "server-wrapper: %v\n", err)
		os.Exit(1)
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}
