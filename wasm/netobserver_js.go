//go:build js && wasm

package main

import "syscall/js"

// networkObserver belongs to one host-authorized grant, never a task message.
type networkObserver struct {
	taskID      string
	grant       int
	active      bool
	callback    js.Value
	connections map[string]map[string]any
	receive     js.Func
}

func newNetworkObserver(taskID string, grant int, callback js.Value) *networkObserver {
	o := &networkObserver{taskID: taskID, grant: grant, active: true, callback: callback, connections: make(map[string]map[string]any)}
	o.receive = js.FuncOf(func(_ js.Value, args []js.Value) any {
		if !o.active || len(args) != 1 {
			return nil
		}
		v := args[0]
		if v.Type() != js.TypeObject || v.IsNull() {
			return nil
		}
		if v.Get("rid").Type() != js.TypeString || v.Get("generation").Type() != js.TypeNumber || v.Get("target").Type() != js.TypeString || v.Get("state").Type() != js.TypeString {
			return nil
		}
		rid := v.Get("rid").String()
		state := v.Get("state").String()
		switch state {
		case "idle", "connecting", "connected", "closed", "error":
		default:
			return nil
		}
		snapshot := map[string]any{"taskId": o.taskID, "grantGeneration": o.grant, "rid": rid, "generation": v.Get("generation").Float(), "target": v.Get("target").String(), "state": state}
		o.connections[rid] = snapshot
		if o.callback.Type() == js.TypeFunction {
			notifyNetworkObserver(o.callback, js.ValueOf(snapshot))
		}
		return nil
	})
	return o
}

// A host UI exception must not unwind a provider lifecycle transition.
func notifyNetworkObserver(callback, snapshot js.Value) {
	defer func() { _ = recover() }()
	callback.Invoke(snapshot)
}

func (o *networkObserver) snapshot() any {
	result := make([]any, 0, len(o.connections))
	if o.active {
		for _, entry := range o.connections {
			result = append(result, entry)
		}
	}
	return result
}

func (o *networkObserver) invalidate() {
	if !o.active {
		return
	}
	o.active = false
	if o.callback.Type() == js.TypeFunction {
		for _, entry := range o.connections {
			final := make(map[string]any, len(entry))
			for key, value := range entry {
				final[key] = value
			}
			final["state"] = "closed"
			notifyNetworkObserver(o.callback, js.ValueOf(final))
		}
	}
	o.callback = js.Undefined()
	clear(o.connections)
}
