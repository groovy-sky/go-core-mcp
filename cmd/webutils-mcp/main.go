package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/groovy-sky/groovy-agent/webutils"
)

func main() {
	flag.Parse()
	logger := log.New(os.Stderr, "webutils-mcp: ", 0)
	server := webutils.NewServer(webutils.DefaultLimits(), nil, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "webutils-mcp: %s\n", err)
		os.Exit(1)
	}
}
