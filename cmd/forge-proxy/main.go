// Command forge-proxy is the reverse proxy that `forge deploy` runs on each
// web host. It routes by Host, obtains TLS certificates through ACME, and
// switches traffic between application containers without dropping
// requests. See docs/DEPLOYMENT.md and ADR 0030.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rocketinsidecorp/forge-go/internal/proxy"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := proxy.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
