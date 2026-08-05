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
		apiAddr    string
		ninePAddr  string
		gojsRunner string
		nodeBinary string
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
Logs always go to stderr, leaving stdin/stdout free for a stdio transport.`,
		Run: func(ctx *cli.Context, args []string) {
			srv := headless.New(headless.Options{
				APIAddr:    apiAddr,
				NinePAddr:  ninePAddr,
				GoJSRunner: gojsRunner,
				NodeBinary: nodeBinary,
			})
			runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := srv.Run(runCtx); err != nil {
				fmt.Fprintf(os.Stderr, "wanix headless: %v\n", err)
				os.Exit(1)
			}
		},
	}
	cmd.Flags().StringVar(&apiAddr, "api", "stdio", "task/API port address (stdio|unix://path|tcp://host:port)")
	cmd.Flags().StringVar(&ninePAddr, "9p", default9PAddr(), "raw 9P stream address (stdio|unix://path|tcp://host:port)")
	cmd.Flags().StringVar(&gojsRunner, "gojs-runner", "", "path to wanix-gojs-runner.mjs (enables native GoJS tasks; requires socket --api)")
	cmd.Flags().StringVar(&nodeBinary, "node", "node", "Node executable for --gojs-runner")
	return cmd
}

// default9PAddr picks a per-user unix socket so a stdio API host still gets 9P.
func default9PAddr() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return "unix://" + filepath.Join(dir, fmt.Sprintf("wanix-9p-%d.sock", os.Getuid()))
}
