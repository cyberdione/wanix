//go:build js && wasm

package worker

import (
	"syscall/js"
	"testing"
	"tractor.dev/wanix"
)

func TestTaskExitTerminatesWorkerAndReleasesCallbacks(t *testing.T) {
	tasks := wanix.NewTaskFS()
	task, _ := tasks.Alloc("auto", nil)
	count := 0
	terminate := js.FuncOf(func(js.Value, []js.Value) any { count++; return nil })
	defer terminate.Release()
	w := js.Global().Get("Object").New()
	w.Set("terminate", terminate)
	r := &Resource{task: task, worker: w, state: "running"}
	wanix.SetWorker(task, w)
	r.callback(func(js.Value, []js.Value) any { return nil })
	task.AddCleanup(r.stop)
	task.Exit("130")
	task.Exit("0")
	if count != 1 || len(r.callbacks) != 0 || r.state != "terminated" || wanix.GetWorker(task) != nil {
		t.Fatal("worker cleanup incomplete")
	}
}
