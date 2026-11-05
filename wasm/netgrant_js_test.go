//go:build js && wasm

package main

import (
	"context"
	"errors"
	"testing"
	"tractor.dev/wanix"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/memfs"
)

type grantDriver struct{}

func (grantDriver) Check(*wanix.Task) bool  { return false }
func (grantDriver) Start(*wanix.Task) error { return nil }

func TestGrantNetworkIsTaskScoped(t *testing.T) {
	tasks := wanix.NewTaskFS()
	root, err := wanix.NewRootWithTasks(tasks)
	if err != nil {
		t.Fatal(err)
	}
	tasks.Register("test", grantDriver{})
	child, err := tasks.Alloc("test", root)
	if err != nil {
		t.Fatal(err)
	}
	service := memfs.New()
	if err := grantNetwork(root, service, child.ID()); err != nil {
		t.Fatal(err)
	}
	t.Logf("task bindings: %s", child.NS().String())
	if _, err := fs.OpenContext(context.Background(), child.NS(), "net/tcp"); err != nil {
		t.Fatalf("granted task cannot see mount: %v", err)
	}
	if _, err := fs.OpenContext(context.Background(), root.NS(), "net/tcp"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("root unexpectedly sees net/tcp: %v", err)
	}
	sibling, err := tasks.Alloc("test", root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.OpenContext(context.Background(), sibling.NS(), "net/tcp"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ungranted sibling sees net/tcp: %v", err)
	}
	if err := grantNetwork(root, service, root.ID()); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("root grant err=%v, want ErrPermission", err)
	}
	if err := revokeNetwork(root, service, child.ID()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := fs.OpenContext(context.Background(), child.NS(), "net/tcp"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("revoked task retains net/tcp mount: %v", err)
	}
	if err := revokeNetwork(root, service, root.ID()); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("root revoke err=%v, want ErrPermission", err)
	}
}
