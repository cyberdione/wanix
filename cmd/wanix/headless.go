//go:build !js && !wasm

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tractor.dev/toolkit-go/engine/cli"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/memfs"
	"tractor.dev/wanix/headless"
)

// headlessCmd exposes the Wanix root task over native transports for a
// non-browser host. See headless/CONTRACT.md for the wiring a host uses.
func headlessCmd() *cli.Command {
	var (
		apiAddr      string
		ninePAddr    string
		gojsRunner   string
		nodeBinary   string
		shellCommand string
		shellWASM    string
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
			if shellCommand != "" {
				root, err := srv.RootTask()
				if err != nil {
					fmt.Fprintf(os.Stderr, "wanix headless: %v\n", err)
					os.Exit(1)
				}
				if shellWASM != "" {
					artifact, err := os.ReadFile(shellWASM)
					if err != nil {
						fmt.Fprintf(os.Stderr, "wanix headless: read shell wasm: %v\n", err)
						os.Exit(1)
					}
					if err := root.NS().Bind(memfs.New(), ".", "bin"); err != nil {
						fmt.Fprintf(os.Stderr, "wanix headless: mount shell wasm: %v\n", err)
						os.Exit(1)
					}
					if err := fs.WriteFile(root.NS(), "bin/rc.wasm", artifact, 0755); err != nil {
						fmt.Fprintf(os.Stderr, "wanix headless: install shell wasm: %v\n", err)
						os.Exit(1)
					}
				}
				task, err := headless.StartCommand(root, shellCommand)
				if err != nil {
					fmt.Fprintf(os.Stderr, "wanix headless: start shell: %v\n", err)
					os.Exit(1)
				}
				go func() {
					code, err := headless.WaitExit(task, 20*time.Millisecond)
					if err != nil {
						fmt.Fprintf(os.Stderr, "wanix headless: shell: %v\n", err)
						stop()
						return
					}
					if code != 0 {
						fmt.Fprintf(os.Stderr, "wanix headless: shell exited %d\n", code)
					}
					stop()
				}()
			}
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
	cmd.Flags().StringVar(&shellCommand, "shell-command", "", "start a native Wanix task (for example: rc.wasm -c 'echo native')")
	cmd.Flags().StringVar(&shellWASM, "shell-wasm", "", "host path to rc.wasm, installed as bin/rc.wasm before --shell-command starts")
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
