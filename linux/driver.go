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
// device itself places no windows: ids are allocated through the host WM
// or wctl new (ADR 0007), and the default openat allowlist admits exactly
// the rio files under /mnt/wsys/<id>/ (never the host srv/ plane).
//
// wsysWMGrant is the WM bind variant (ADR 0007 K1): the WM task binds
// "#wsys/wm" instead of "#wsys", which adds the srv plane — the
// srv/state enumeration and per-window srv operations — for that task
// only. A task is the WM iff its launch env carries wsysWMEnv (set by the
// display server's launch wiring, read here and mirrored by the worker's
// openat allowlist); there is no other mechanism, and no task can set it
// for itself through the personality.
const (
	drawDevice = "#i"
	drawMount  = "dev"

	wsysDevice  = "#wsys"
	wsysWMGrant = "#wsys/wm"
	wsysMount   = "mnt/wsys"

	wsysWMEnv = "WSYS_WM"
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
	// The bind source carries the grant: the WM task (launch env
	// WSYS_WM=1, set by the display server's launch wiring) binds
	// "#wsys/wm" and its worker allowlist admits the srv plane; every
	// other task binds "#wsys" and keeps the default grant.
	src := wsysDevice
	for _, kv := range t.Env() {
		if kv == wsysWMEnv+"=1" {
			src = wsysWMGrant
			break
		}
	}
	if err := t.Bind(src, wsysMount); err != nil {
		return err
	}
	return worker.StartTaskWorker(d.Workers, t, linuxworker.BlobURL())
}
