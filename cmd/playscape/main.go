// Command playscape is the control plane binary. All logic lives in
// internal/cli; this file only carries the build-time version metadata.
package main

import (
	"os"

	"github.com/urmzd/playscape/internal/cli"
)

// Injected via -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "0.0.0-dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(cli.Execute(cli.Version{Version: version, Commit: commit, Date: date}))
}
