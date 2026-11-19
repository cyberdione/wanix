//go:build js && wasm

package linux

import (
	"tractor.dev/wanix"
	linuxworker "tractor.dev/wanix/linux/worker"
	"tractor.dev/wanix/misc/wasmutil"
	"tractor.dev/wanix/web/worker"
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
	return worker.StartTaskWorker(d.Workers, t, linuxworker.BlobURL())
}
