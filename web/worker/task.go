//go:build js && wasm

package worker

import (
	"fmt"
	"strings"
	"syscall/js"

	"tractor.dev/wanix"
)

func FromTask(t *wanix.Task) js.Value {
	w := wanix.GetWorker(t)
	if w == nil {
		return js.Undefined()
	}
	return w.(js.Value)
}

func StartTaskWorker(svc *Device, t *wanix.Task, blobURL string) error {
	profile := t.DiagnosticProfile()
	if profile != "" && profile != wanix.Capture2DiagnosticProfile {
		return fmt.Errorf("unsupported diagnostic profile")
	}
	scenario := t.MemoryControlScenario()
	if scenario != "" && (profile != "" || !wanix.ValidMemoryControlScenario(scenario)) {
		return fmt.Errorf("unsupported memory control selection")
	}
	w, err := svc.Alloc(t)
	if err != nil {
		return err
	}
	w.diagnosticProfile = profile
	w.memoryControlScenario = scenario
	args := append([]string{blobURL}, strings.Split(t.Cmd(), " ")...)
	return w.Start(args...)
}
