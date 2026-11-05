//go:build js && wasm

package main

import (
	"syscall/js"
	"testing"
)

func TestNetworkObserverGrantIdentityAndRevocation(t *testing.T) {
	var received js.Value
	cb := js.FuncOf(func(_ js.Value, args []js.Value) any {
		received = args[0]
		args[0].Set("target", "tampered")
		return nil
	})
	defer cb.Release()
	a := newNetworkObserver("trusted-task", 7, cb.Value)
	defer a.receive.Release()
	b := newNetworkObserver("other-task", 8, js.Undefined())
	defer b.receive.Release()
	event := map[string]any{"rid": "0", "generation": 2, "target": "host:22", "state": "connected", "taskId": "forged", "grantGeneration": 99}
	a.receive.Invoke(event)
	b.receive.Invoke(event)
	if received.Get("taskId").String() != "trusted-task" || received.Get("grantGeneration").Int() != 7 {
		t.Fatal("identity was not pinned")
	}
	if a.connections["0"]["target"] != "host:22" || b.connections["0"]["taskId"] != "other-task" {
		t.Fatal("observer state not isolated/copied")
	}
	a.invalidate()
	a.receive.Invoke(event)
	replacement := newNetworkObserver("trusted-task", 9, js.Undefined())
	defer replacement.receive.Release()
	if len(replacement.connections) != 0 {
		t.Fatal("replacement inherited snapshots")
	}
	replacement.receive.Invoke(event)
	if replacement.connections["0"]["grantGeneration"] != 9 {
		t.Fatal("replacement generation was not pinned")
	}
	if len(a.connections) != 0 || len(b.connections) != 1 {
		t.Fatal("revoked observer accepts late events or clears other grant")
	}
}
