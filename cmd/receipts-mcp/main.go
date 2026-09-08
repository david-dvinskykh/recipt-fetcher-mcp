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
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/authweb"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/config"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/mcpserver"
)

func main() {
	var (
		showVersion = flag.Bool("version", false, "print the version and exit")
		showStatus  = flag.Bool("status", false, "print provider status as JSON and exit, without serving MCP")
		stateDir    = flag.String("state-dir", "", "where credentials are stored (overrides "+config.EnvStateDir+")")
		httpAddr    = flag.String("http", "", "serve MCP over Streamable HTTP at /mcp plus the login web app at /login on this address (e.g. :8390); default is stdio")
		chromium    = flag.String("chromium", os.Getenv("RECEIPTS_CHROMIUM"), "path to a Chromium/Chrome for the guided login (defaults to one on PATH)")
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

	if *httpAddr != "" {
		if err := serveHTTP(ctx, *httpAddr, server, *chromium, cfg.StateDir); err != nil {
			log.Fatalf("server stopped: %v", err)
		}
		return
	}

	log.Printf("serving MCP over stdio (state dir %s)", cfg.StateDir)
	if err := server.MCPServer().Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

// serveHTTP runs MCP over Streamable HTTP at /mcp alongside the login web app.
func serveHTTP(ctx context.Context, addr string, server *mcpserver.Server, chromium, stateDir string) error {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server.MCPServer()
	}, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)
	authweb.New(server, chromium).Mount(mux, "/login")

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("serving MCP at %s/mcp and login at %s/login (state dir %s)", addr, addr, stateDir)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
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
