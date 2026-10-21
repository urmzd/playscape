// Command playscape validates a command in a playscape sandbox, so a person
// can see what a policy lets it read, what it may change, and what failed.
//
//	playscape run [flags] COMMAND
//	playscape version
//
// It is a tool for people, not a service: it opens no listener and exits
// when the command does. Programs embed the library instead (see the
// README).
package main

import (
	"fmt"
	"io"
	"os"
)

// Injected via -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "0.0.0-dev"
	commit  = "none"
	date    = "unknown"
)

// Exit codes of playscape itself. A command that ran and exited reports its
// own exit code instead.
const (
	exitUsage = 2 // bad flags or arguments
	// exitDenied is EX_NOPERM from sysexits.h: the command succeeded, but
	// the policy refused some of the workspace changes it made.
	exitDenied = 77
	// exitTimeout matches timeout(1).
	exitTimeout = 124
	// exitFailed means playscape could not run the command at all.
	exitFailed = 125
	// exitInterrupted is 128 + SIGINT.
	exitInterrupted = 130
)

const usage = `playscape validates a command in a playscape sandbox.

Usage:
  playscape run [flags] COMMAND   run COMMAND under a workspace policy and report what happened
  playscape version               print the version

Run "playscape run -h" for the flags of run.
`

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

// execute runs the command line args and returns the process exit code.
func execute(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "run":
		return runCommand(args[1:], stdout, stderr)
	case "version", "-version", "--version":
		fmt.Fprintf(stdout, "playscape v%s (commit %s, built %s)\n", version, commit, date)
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "playscape: unknown command %q\n\n%s", args[0], usage)
		return exitUsage
	}
}
