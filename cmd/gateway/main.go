// Command gateway is the Anselm free-tier inference gateway.
// It imports ONLY internal/bootstrap (the composition root): the localhost-only
// ban/unban admin CLI, else build + run the three-listener server.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sunweilin/anselm/gateway/internal/bootstrap"
)

func main() {
	// SEC-2 admin subcommands (ban/unban) run AS A CLI on the box (over SSH), never
	// as the long-running server. Dispatch them FIRST and exit; a non-admin arg
	// falls through to serve.
	if handled, code := bootstrap.RunCLI(os.Args, os.Getenv); handled {
		os.Exit(code)
	}

	app, err := bootstrap.Build(context.Background(), os.Getenv)
	if err != nil {
		// Logger may not be up on an early config failure; stderr is the floor.
		fmt.Fprintf(os.Stderr, "gateway: build failed: %v\n", err)
		os.Exit(1)
	}
	if err := bootstrap.Run(app); err != nil {
		os.Exit(1)
	}
}
