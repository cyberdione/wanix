//go:build !js && !wasm

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"tractor.dev/toolkit-go/engine/cli"
	"tractor.dev/wanix/headless"
)

// headlessCmd exposes the Wanix root task over native transports for a
// non-browser host. See headless/CONTRACT.md for the wiring a host uses.
func headlessCmd() *cli.Command {
	var (
		apiAddr   string
		ninePAddr string
	)
	cmd := &cli.Command{
		Usage: "headless",
		Short: "serve Wanix over native API and 9P transports (no browser)",
		Long: `Serve a Wanix root task over two native duplex transports:

  --api   task/API port (duplex mux/talk over CBOR); same primitives the
          browser reaches through the worker's _openPort channel.
  --9p    raw full-duplex 9P stream bound to the root task namespace; same
          primitives the browser reaches through the worker's _open9P channel.

Addresses use the schemes: stdio, unix://<path>, tcp://<host:port>.
At most one of the two may be "stdio" (only one stdin/stdout pair exists).
Logs always go to stderr, leaving stdin/stdout free for a stdio transport.

Unix sockets are forced to mode 0600 after bind: they grant full access to
the served namespace, so they are owner-only regardless of umask.
tcp:// provides no authentication and is refused unless
WANIX_ALLOW_INSECURE_TCP=1. Without XDG_RUNTIME_DIR, the default 9P socket
path falls back to /tmp and the command refuses to start unless
WANIX_ALLOW_INSECURE_SOCKET_DIR=1.`,
		Run: func(ctx *cli.Context, args []string) {
			if err := checkDefault9PDir(ninePAddr); err != nil {
				fmt.Fprintf(os.Stderr, "wanix headless: %v\n", err)
				os.Exit(1)
			}
			srv := headless.New(headless.Options{
				APIAddr:   apiAddr,
				NinePAddr: ninePAddr,
			})
			runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := srv.Run(runCtx); err != nil {
				fmt.Fprintf(os.Stderr, "wanix headless: %v\n", err)
				os.Exit(1)
			}
		},
	}
	cmd.Flags().StringVar(&apiAddr, "api", "stdio", "task/API port address (stdio|unix://path|tcp://host:port; tcp needs WANIX_ALLOW_INSECURE_TCP=1)")
	cmd.Flags().StringVar(&ninePAddr, "9p", default9PAddr(), "raw 9P stream address (stdio|unix://path|tcp://host:port; tcp needs WANIX_ALLOW_INSECURE_TCP=1)")
	return cmd
}

// checkDefault9PDir refuses the world-writable /tmp fallback for the default
// 9P socket: a default path in a shared sticky directory invites
// symlink/squatting races on a socket that grants full namespace access.
// The operator must set XDG_RUNTIME_DIR, pass an explicit --9p unix:// path
// in a private directory, or opt in explicitly.
func checkDefault9PDir(addr string) error {
	if addr != default9PAddr() || os.Getenv("XDG_RUNTIME_DIR") != "" {
		return nil
	}
	if os.Getenv("WANIX_ALLOW_INSECURE_SOCKET_DIR") == "1" {
		return nil
	}
	return fmt.Errorf("XDG_RUNTIME_DIR is unset: the default 9P socket would be created in %s (world-writable); set XDG_RUNTIME_DIR to a private directory, pass an explicit --9p unix:// path inside one, or set WANIX_ALLOW_INSECURE_SOCKET_DIR=1 to accept the risk", os.TempDir())
}

// default9PAddr picks a per-user unix socket so a stdio API host still gets 9P.
func default9PAddr() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return "unix://" + filepath.Join(dir, fmt.Sprintf("wanix-9p-%d.sock", os.Getuid()))
}
