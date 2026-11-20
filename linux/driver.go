//go:build js && wasm

package linux

import (
	"tractor.dev/wanix"
	"tractor.dev/wanix/fs"
	linuxworker "tractor.dev/wanix/linux/worker"
	"tractor.dev/wanix/misc/wasmutil"
	"tractor.dev/wanix/web/worker"
)

// drawDevice is the kernel draw(3) device, registered at #i (wasm/wasm.go
// sysbindings). drawMount is where every linux task's namespace binds it:
// Plan 9's init(8) `bind -a #i /dev` serving /dev/draw.
//
// wsysDevice is the kernel rio(4) window-service device, registered at
// #wsys (fs/wsys). wsysMount is where every linux task's namespace binds
// it, the per-window files (cons, consctl, mouse, label, wctl) under
// /mnt/wsys/<id>/ — rio's wsys mount, the same bind pattern as #i. The
// device itself places no windows: ids are allocated by the host WM
// (the desktop session) and the openat allowlist admits exactly the rio
// files under mnt/wsys/<id>/ (never the host srv/ plane).
const (
	drawDevice = "#i"
	drawMount  = "dev"

	wsysDevice = "#wsys"
	wsysMount  = "mnt/wsys"
)

// Driver runs Linux-personality wasm32 tasks (ferrule.linux.syscall import)
// such as the musl-based FerruleOS userland artifacts.
type Driver struct {
	Workers *worker.Device
}

func (d *Driver) Check(t *wanix.Task) bool {
	typ, err := wasmutil.DetectType(t.NS(), t.Arg(0))
	if err != nil {
		return false
	}
	return typ == "linux"
}

func (d *Driver) Start(t *wanix.Task) error {
	// Serve /dev/draw in the task namespace: bind #i after dev, exactly as
	// Plan 9 init(8) does. Namespaces are cloned per task at alloc, so this
	// bind reaches only this task; the worker's openat allowlist admits the
	// device files it exposes (/dev/draw/new and /dev/draw/<n>/*). A missing
	// draw device is a broken kernel build, not a per-task condition, so the
	// bind failure fails the task start.
	if err := t.Bind(drawDevice, drawMount, fs.BindAfter); err != nil {
		return err
	}
	// Serve the rio window service at /mnt/wsys. Like #i, a missing device
	// is a broken kernel build, so the bind failure fails the task start.
	if err := t.Bind(wsysDevice, wsysMount); err != nil {
		return err
	}
	return worker.StartTaskWorker(d.Workers, t, linuxworker.BlobURL())
}
