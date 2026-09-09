// Command agent is the CLI entry point: it connects a local llama-server to
// the coreutils MCP server and answers a single request.
//
// Diagnostics are written to stderr; only the final answer reaches stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/groovy-sky/groovy-agent/internal/agent"
)

func main() {
	config := agent.Config{}
	flags := flag.NewFlagSet("agent", flag.ExitOnError)
	flags.StringVar(&config.LlamaURL, "llama-url", "http://127.0.0.1:8080", "base URL of the local llama-server")
	flags.StringVar(&config.Model, "model", "local-phi-4-mini-instruct", "model name advertised by llama-server")
	flags.StringVar(&config.MCPCommand, "mcp-command", "./bin/coreutils-mcp", "path to the coreutils MCP server executable")
	flags.StringVar(&config.WebMCPCommand, "web-mcp-command", "", "optional path to the webutils MCP server executable")
	flags.StringVar(&config.Workspace, "workspace", ".", "workspace directory that bounds every filesystem operation")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	config.Prompt = strings.TrimSpace(strings.Join(flags.Args(), " "))
	if config.Prompt == "" {
		fmt.Fprintln(os.Stderr, "agent: a prompt argument is required")
		flags.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := agent.Run(ctx, config, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "agent: %s\n", err)
		os.Exit(1)
	}
}
