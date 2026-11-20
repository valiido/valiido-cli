package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/valiido/valiido-cli/internal/cli"
)

func main() {
	ignoreBrokenPipe()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	cli.NotifyUpdate(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}
