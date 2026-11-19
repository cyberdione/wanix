package wanix

import (
	"errors"
	"testing"

	"tractor.dev/wanix/fs"
)

func TestTaskWritableFSStableAndIsolated(t *testing.T) {
	root, err := NewRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Exit("0")
	child, err := root.fsys.Alloc("auto", root)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Exit("0")

	for _, task := range []*Task{root, child} {
		name := "#task/" + task.ID() + "/w"
		info, err := fs.Stat(root.NS(), name)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", name)
		}
		if err := fs.WriteFile(root.NS(), name+"/owned", []byte(task.ID()), 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	for _, task := range []*Task{root, child} {
		// Each lookup reconstructs the task map but must retain its writable FS.
		for i := 0; i < 2; i++ {
			data, err := fs.ReadFile(root.NS(), "#task/"+task.ID()+"/w/owned")
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != task.ID() {
				t.Fatalf("task %s read %q", task.ID(), data)
			}
		}
	}
	if err := fs.WriteFile(child.NS(), "#task/"+child.ID()+"/w/child-only", []byte("child"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(root.NS(), "#task/"+root.ID()+"/w/child-only"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("child file leaked to parent: %v", err)
	}
}
