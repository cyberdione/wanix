//go:build !js && !wasm

package native

// Native GoJS execution deliberately lives in Wanix: the Node runner relays
// the existing gojs worker's MessagePort frames to api.Responder, so every
// filesystem and task operation remains a Wanix API call.

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"

	"tractor.dev/wanix"
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
	if d.Runner == "" || d.APIAddr == "" || d.APIAddr == "stdio" {
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
	cmd := exec.CommandContext(t.Context(), node, d.Runner, "--api", d.APIAddr, "--tid", t.ID())
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start native GoJS runner: %w", err)
	}
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
