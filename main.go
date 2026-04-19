package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "0.2.3"

func main() {
	// CLI flag parsing
	connectAddr := flag.String("connect", "", "TCP address of existing dlv --headless DAP server (e.g. localhost:24010 after kubectl port-forward)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	// Env fallback (ADR-9: CLI has precedence over env)
	addr := *connectAddr
	if addr == "" {
		addr = os.Getenv("DAP_CONNECT_ADDR")
	}

	// Log to a file tagged by the k8s target (if known) and PID so multiple
	// concurrent mcp-dap-server instances don't clobber each other's logs
	// and operators can find the right file without guessing PIDs.
	//
	// DLV_NAMESPACE + DLV_SERVICE come from the wrapper script (dlv-k8s-mcp.sh)
	// and get inherited. Fallback to PID-only naming when they aren't set
	// (local dlv/gdb spawn, direct invocation, etc.).
	//
	// Never write to stderr — with MCP stdio transport stderr is a pipe to
	// the MCP client, and a full pipe buffer would block the logging goroutine
	// and hang the server.
	tag := ""
	if ns, svc := os.Getenv("DLV_NAMESPACE"), os.Getenv("DLV_SERVICE"); ns != "" && svc != "" {
		tag = ns + "-" + svc + "."
	}
	logPath := filepath.Join(os.TempDir(), fmt.Sprintf("mcp-dap-server.%s%d.log", tag, os.Getpid()))
	var logWriter io.Writer
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		logWriter = io.Discard
		log.SetOutput(logWriter)
	} else {
		logWriter = logFile
		log.SetOutput(logWriter)
		defer logFile.Close()
	}

	// Microsecond-resolution timestamps make it possible to measure short
	// DAP round-trips in the log. The prefix carries PID and (if present) the
	// remote DAP target, so grep'ing across multiple session logs is useful.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	prefix := fmt.Sprintf("[pid=%d", os.Getpid())
	if addr != "" {
		prefix += " addr=" + addr
	}
	prefix += "] "
	log.SetPrefix(prefix)

	log.Printf("mcp-dap-server starting (version=%s log=%s connect=%q level=%s)",
		version, logPath, addr, currentLogLevel.String())

	// Maintain a convenience "latest" symlink per target tag so running two
	// wrapped mcp-dap-server instances (e.g. server + ca) each get their own
	// latest.log that points at that tag's current session. Untagged fallback
	// uses mcp-dap-server.latest.log as before. Best-effort — failures logged
	// but not fatal.
	latestPath := filepath.Join(os.TempDir(), fmt.Sprintf("mcp-dap-server.%slatest.log", tag))
	_ = os.Remove(latestPath)
	if err := os.Symlink(logPath, latestPath); err != nil {
		log.Printf("warn: could not create latest.log symlink: %v", err)
	}

	// SIGUSR1 handler: dumps a full goroutine profile to the log. Use when
	// the server appears hung: `pkill -USR1 mcp-dap-server` surfaces where
	// every goroutine is parked without terminating the process.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)
	go func() {
		for range sigCh {
			log.Printf("=== SIGUSR1 goroutine dump ===")
			if p := pprof.Lookup("goroutine"); p != nil {
				// Level 2 gives verbose output including goroutine addresses
				// and stacks — identical format to an unrecovered panic dump,
				// which is what developers are used to reading.
				_ = p.WriteTo(logFile, 2)
			}
			log.Printf("=== SIGUSR1 dump complete ===")
		}
	}()

	// Create MCP server
	implementation := mcp.Implementation{
		Name:    "mcp-dap-server",
		Version: version,
	}
	server := mcp.NewServer(&implementation, nil)

	ds := registerTools(server, logWriter, addr)
	defer ds.cleanup()

	registerPrompts(server)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
