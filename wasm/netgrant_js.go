//go:build js && wasm

package main

import (
	"tractor.dev/wanix"
	"tractor.dev/wanix/fs"
)

// grantNetwork exposes the session-owned TCP service only in one task's
// cloned namespace. The service must not be bound into the root namespace.
func grantNetwork(root *wanix.Task, service fs.FS, taskID string) error {
	task, err := root.Lookup(taskID)
	if err != nil {
		return err
	}
	if task == root {
		return fs.ErrPermission
	}
	return task.NS().Bind(service, ".", "net/tcp")
}

// revokeNetwork removes the TCP service from one task's namespace.
func revokeNetwork(root *wanix.Task, service fs.FS, taskID string) error {
	task, err := root.Lookup(taskID)
	if err != nil {
		return err
	}
	if task == root {
		return fs.ErrPermission
	}
	return task.NS().Unbind(service, ".", "net/tcp")
}
