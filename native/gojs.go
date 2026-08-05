//go:build !js && !wasm

package native

// Native GoJS execution deliberately lives in Wanix: the Node runner relays
// the existing gojs worker's MessagePort frames to api.Responder, so every
// filesystem and task operation remains a Wanix API call.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"tractor.dev/toolkit-go/duplex/mux"
	"tractor.dev/wanix"
	"tractor.dev/wanix/api"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/misc/wasmutil"
)

// GoJSDriver runs GoJS Wasm tasks with the packaged Node bridge. Runner must
// be the `wanix-gojs-runner.mjs` asset and APIAddr must be a dialable headless
// API socket (not stdio).
type GoJSDriver struct {
	Node    string
	Runner  string
	APIAddr string
}

var _ wanix.TaskDriver = (*GoJSDriver)(nil)

func (d *GoJSDriver) Check(t *wanix.Task) bool {
	if d.Runner == "" {
		return false
	}
	typ, err := wasmutil.DetectType(t.NS(), t.Arg(0))
	return err == nil && typ == "gojs"
}

func (d *GoJSDriver) Start(t *wanix.Task) error {
	node := d.Node
	if node == "" {
		node = "node"
	}
	listener, socket, err := taskAPIListener(t)
	if err != nil {
		return err
	}
	defer func() {
		if listener != nil {
			_ = listener.Close()
			_ = os.Remove(socket)
		}
	}()
	go serveTaskAPI(listener, t)
	cmd := exec.CommandContext(t.Context(), node, d.Runner, "--api", "unix://"+socket, "--tid", t.ID())
	if stderr, _, err := t.FD(2); err == nil {
		if writer, ok := stderr.(io.Writer); ok {
			cmd.Stderr = writer
		}
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start native GoJS runner: %w", err)
	}
	listener = nil // serveTaskAPI owns it after the child started.
	wanix.SetWorker(t, cmd.Process)
	go func() {
		err := cmd.Wait()
		if err == nil {
			return // the GoJS worker writes #task/<id>/exit itself.
		}
		code := 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		f, openErr := t.Open("exit")
		if openErr == nil {
			_, _ = fs.Write(f, []byte(strconv.Itoa(code)))
			_ = f.Close()
		}
	}()
	return nil
}

func taskAPIListener(t *wanix.Task) (net.Listener, string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	socket := filepath.Join(dir, fmt.Sprintf("wanix-gojs-%d-%s.sock", os.Getpid(), t.ID()))
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("remove stale GoJS API socket: %w", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, "", fmt.Errorf("listen GoJS task API socket: %w", err)
	}
	return listener, socket, nil
}

func serveTaskAPI(listener net.Listener, task *wanix.Task) {
	defer listener.Close()
	defer os.Remove(listener.Addr().String())
	conn, err := listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	api.Responder(mux.New(conn), task)
}
