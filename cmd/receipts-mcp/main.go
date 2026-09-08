// Command receipts-mcp serves shop receipts to an MCP client over stdio.
//
// Stores supported: Lidl Plus, Action and Allegro. See README.md for how to
// obtain the credentials each one needs.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/config"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/mcpserver"
)

func main() {
	var (
		showVersion = flag.Bool("version", false, "print the version and exit")
		showStatus  = flag.Bool("status", false, "print provider status as JSON and exit, without serving MCP")
		stateDir    = flag.String("state-dir", "", "where credentials are stored (overrides "+config.EnvStateDir+")")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("receipts-mcp", mcpserver.Version)
		return
	}

	// Logs go to stderr: stdout is the MCP transport.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("receipts-mcp ")

	cfg := config.Load()
	if *stateDir != "" {
		cfg.StateDir = *stateDir
	}

	server, err := mcpserver.New(cfg)
	if err != nil {
		log.Fatalf("startup failed: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *showStatus {
		if err := printStatus(ctx, server); err != nil {
			log.Fatalf("status failed: %v", err)
		}
		return
	}

	log.Printf("serving MCP over stdio (state dir %s)", cfg.StateDir)
	if err := server.MCPServer().Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

// printStatus is the way to check credentials from a shell, without an MCP client.
func printStatus(ctx context.Context, server *mcpserver.Server) error {
	status, err := server.StatusReport(ctx)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(status)
}
