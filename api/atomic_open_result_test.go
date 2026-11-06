package api

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"tractor.dev/toolkit-go/duplex/codec"
	"tractor.dev/toolkit-go/duplex/mux"
	"tractor.dev/toolkit-go/duplex/talk"
	"tractor.dev/wanix"
	"tractor.dev/wanix/fs"
)

type atomicTestDriver struct{}

func (atomicTestDriver) Check(*wanix.Task) bool  { return false }
func (atomicTestDriver) Start(*wanix.Task) error { return nil }

func TestExistingAtomicOpenSelectedRPC(t *testing.T) {
	root, err := wanix.NewRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Exit("done")
	root.Register("linux", atomicTestDriver{})
	selected, err := fs.ReadFile(root.NS(), "#task/new/linux-scratch")
	if err != nil {
		t.Fatal(err)
	}
	task, err := root.Lookup(strings.TrimSpace(string(selected)))
	if err != nil {
		t.Fatal(err)
	}
	defer task.Exit("done")
	base := "#task/" + task.ID() + "/w/"
	if err := fs.WriteFile(root.NS(), base+"out.txt", []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	old, err := fs.OpenFile(root.NS(), base+"out.txt", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	before, _ := old.Stat()
	a, b := mux.Pair()
	t.Cleanup(func() { a.Close(); b.Close() })
	go Responder(a, task)
	client := talk.NewPeer(b, codec.CBORCodec{})
	request := func(path, access string, trunc bool) map[string]any {
		return map[string]any{"Path": path, "Access": access, "Create": false, "Exclusive": false, "Truncate": trunc, "Perm": uint64(0)}
	}
	call := func(req any, code string) filesystemResult[uint64] {
		t.Helper()
		var got filesystemResult[uint64]
		if _, err := client.Call(context.Background(), "OpenFileResult", req, &got); err != nil {
			t.Fatal(err)
		}
		if code == "" {
			if !got.OK || got.Value == nil || got.Error != nil {
				t.Fatalf("success: %#v", got)
			}
		} else if got.OK || got.Value != nil || got.Error == nil || got.Error.Code != code {
			t.Fatalf("want %s: %#v", code, got)
		}
		return got
	}
	opened := call(request(base+"out.txt", "read", false), "")
	f, _, err := task.FD(int(*opened.Value))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "original" {
		t.Fatalf("preserve %q %v", data, err)
	}
	if _, err := f.(io.WriterAt).WriteAt(nil, 0); err == nil {
		t.Fatal("readonly empty write accepted")
	}
	call(request(base+"out.txt", "read", true), "EINVAL")
	unchanged, _ := old.Stat()
	if unchanged.Size() != before.Size() || !unchanged.ModTime().Equal(before.ModTime()) {
		t.Fatal("readonly trunc mutated")
	}
	truncated := call(request(base+"out.txt", "readwrite", true), "")
	now, _ := old.Stat()
	if now.Size() != 0 || now.Sys() != before.Sys() {
		t.Fatal("trunc replaced inode or retained stale data")
	}
	fresh, _, _ := task.FD(int(*truncated.Value))
	if _, err := old.(io.WriterAt).WriteAt([]byte("A"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.(io.WriterAt).WriteAt([]byte("B"), 1); err != nil {
		t.Fatal(err)
	}
	if err := task.CloseFD(int(*truncated.Value)); err != nil {
		t.Fatal(err)
	}
	data, err = fs.ReadFile(root.NS(), base+"out.txt")
	if err != nil || string(data) != "AB" {
		t.Fatalf("live writes %q %v", data, err)
	}
	// Unlink keeps the API-open inode alive, independent from its replacement.
	retained := call(request(base+"out.txt", "readwrite", false), "")
	retainedFile, _, _ := task.FD(int(*retained.Value))
	if err := fs.Remove(root.NS(), base+"out.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(root.NS(), base+"out.txt", []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := retainedFile.(io.WriterAt).WriteAt([]byte("Z"), 0); err != nil {
		t.Fatal(err)
	}
	replacement, _ := fs.ReadFile(root.NS(), base+"out.txt")
	if string(replacement) != "replacement" {
		t.Fatal("retained fd changed replacement")
	}
	call(nil, "EINVAL")
	call([]any{}, "EINVAL")
	call(request(base+"tmpfile_missing", "read", false), "ENOENT")
	call(request("#task/"+root.ID()+"/w/out.txt", "readwrite", true), "EACCES")
	call(request(base+"../out.txt", "read", false), "EACCES")
	call(request("#task/self/w/out.txt", "read", false), "EACCES")
	req := request(base+"out.txt", "readwrite", false)
	req["Create"] = true
	call(req, "")
	req = request(base+"out.txt", "read", false)
	req["Append"] = true
	call(req, "EINVAL")
	req = request(base+"out.txt", "read", false)
	req["Perm"] = -1
	call(req, "EINVAL")
	req = request(base+"out.txt", "read", false)
	delete(req, "Perm")
	call(req, "EINVAL")
	req = request(base+"out.txt", "read", false)
	req["Perm"] = uint64(1) << 40
	call(req, "EINVAL")
	if err := fs.Mkdir(root.NS(), base+"tmpfile_dir", 0700); err != nil {
		t.Fatal(err)
	}
	call(request(base+"tmpfile_dir", "read", false), "EISDIR")
	// Wrong-relative-leaf binding to the same owned backend must not convey authority.
	if err := task.NS().Bind(task.NS(), base+"out.txt", base+"tmpfile_other", fs.BindReplace); err != nil {
		t.Fatal(err)
	}
	call(request(base+"tmpfile_other", "readwrite", true), "EACCES")
	blocked := request(base+"tmpfile_other", "write", true)
	blocked["Create"] = true
	blocked["Perm"] = uint64(0777)
	call(blocked, "EACCES")
	// Missing leaf overlay is denied too; never fall back to the owned parent.
	if err := task.NS().Bind(task.NS(), base+"out.txt", base+"tmpfile_absent_overlay", fs.BindReplace); err != nil {
		t.Fatal(err)
	}
	blocked["Path"] = base + "tmpfile_absent_overlay"
	call(blocked, "EACCES")
	otherID, err := fs.ReadFile(root.NS(), "#task/new/linux-scratch")
	if err != nil {
		t.Fatal(err)
	}
	other, err := root.Lookup(strings.TrimSpace(string(otherID)))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Exit("done")
	foreign := "#task/" + other.ID() + "/w"
	if err := fs.WriteFile(root.NS(), foreign+"/out.txt", []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := task.NS().Bind(task.NS(), foreign, strings.TrimSuffix(base, "/"), fs.BindReplace); err != nil {
		t.Fatal(err)
	}
	call(request(base+"out.txt", "readwrite", true), "EACCES")
	// A missing leaf must not fall through a foreign parent overlay and create
	// a hidden inode in owned scratch. Neither backend may be changed.
	blocked["Path"] = base + "tmpfile_foreign_missing"
	call(blocked, "EACCES")
	if _, err := fs.Stat(root.NS(), foreign+"/tmpfile_foreign_missing"); err == nil {
		t.Fatal("created foreign missing leaf")
	}
	// Root's namespace still addresses the unoverlaid owned backend.
	if _, err := fs.Stat(root.NS(), base+"tmpfile_foreign_missing"); err == nil {
		t.Fatal("created hidden owned leaf")
	}
	data, _ = fs.ReadFile(root.NS(), foreign+"/out.txt")
	if string(data) != "foreign" {
		t.Fatal("foreign backend mutated")
	}
	task.Exit("done")
	if _, _, err := task.FD(int(*opened.Value)); err == nil {
		t.Fatal("exit retained fd")
	}
}

func TestAtomicCreateSelectedRPC(t *testing.T) {
	root, err := wanix.NewRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Exit("done")
	root.Register("linux", atomicTestDriver{})
	selected, err := fs.ReadFile(root.NS(), "#task/new/linux-scratch")
	if err != nil {
		t.Fatal(err)
	}
	task, err := root.Lookup(strings.TrimSpace(string(selected)))
	if err != nil {
		t.Fatal(err)
	}
	defer task.Exit("done")
	base := "#task/" + task.ID() + "/w/"
	a, b := mux.Pair()
	t.Cleanup(func() { a.Close(); b.Close() })
	go Responder(a, task)
	client := talk.NewPeer(b, codec.CBORCodec{})
	call := func(leaf, access string, create, excl, trunc bool, perm uint64) filesystemResult[uint64] {
		t.Helper()
		var got filesystemResult[uint64]
		_, e := client.Call(context.Background(), "OpenFileResult", map[string]any{"Path": base + leaf, "Access": access, "Create": create, "Exclusive": excl, "Truncate": trunc, "Perm": perm}, &got)
		if e != nil {
			t.Error(e)
		}
		return got
	}
	check := func(got filesystemResult[uint64], code string) {
		t.Helper()
		if code == "" {
			if !got.OK || got.Value == nil {
				t.Fatalf("success: %#v", got)
			}
		} else if got.OK || got.Error == nil || got.Error.Code != code {
			t.Fatalf("want %s: %#v", code, got)
		}
	}
	// Barrier-start requests use the actual RPC method and one unchanged absent leaf.
	start := make(chan struct{})
	results := make(chan filesystemResult[uint64], 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; results <- call("tmpfile_race", "readwrite", true, true, false, 0640) }()
	}
	close(start)
	wins, exists := 0, 0
	for i := 0; i < 2; i++ {
		got := <-results
		if got.OK {
			wins++
		} else if got.Error != nil && got.Error.Code == "EEXIST" {
			exists++
		} else {
			t.Fatalf("race: %#v", got)
		}
	}
	if wins != 1 || exists != 1 {
		t.Fatalf("wins=%d exists=%d", wins, exists)
	}
	info, err := fs.Stat(task.NS(), base+"tmpfile_race")
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("mode: %v %v", info, err)
	}
	for _, mode := range []uint64{0, 0700, 0777} {
		leaf := fmt.Sprintf("tmpfile_mode%d", mode)
		got := call(leaf, "read", true, false, false, mode)
		check(got, "")
		h, _, e := task.FD(int(*got.Value))
		if e != nil {
			t.Fatal(e)
		}
		st, e := h.Stat()
		if e != nil || uint64(st.Mode().Perm()) != mode {
			t.Fatalf("mode %v %v", st, e)
		}
		if _, e = h.(io.WriterAt).WriteAt([]byte("no"), 0); e == nil {
			t.Fatal("readonly create writable")
		}
	}
	if e := fs.WriteFile(task.NS(), base+"out.txt", []byte("keep"), 0611); e != nil {
		t.Fatal(e)
	}
	before, _ := fs.Stat(task.NS(), base+"out.txt")
	check(call("out.txt", "readwrite", true, false, false, 0777), "")
	after, _ := fs.Stat(task.NS(), base+"out.txt")
	if before.Sys() != after.Sys() || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) || after.Size() != 4 {
		t.Fatal("existing create changed inode")
	}
	check(call("out.txt", "write", true, true, true, 0), "EEXIST")
	check(call("out.txt", "read", true, false, true, 0), "EINVAL")
	data, _ := fs.ReadFile(task.NS(), base+"out.txt")
	if string(data) != "keep" {
		t.Fatal("rejected trunc changed bytes")
	}
	check(call("out.txt", "write", true, false, true, 0), "")
	after, _ = fs.Stat(task.NS(), base+"out.txt")
	if before.Sys() != after.Sys() || before.Mode() != after.Mode() || after.Size() != 0 {
		t.Fatal("truncate replaced inode/mode")
	}
	check(call("tmpfile_missing", "read", false, false, false, 0), "ENOENT")
	if e := fs.Mkdir(task.NS(), base+"tmpfile_dir", 0710); e != nil {
		t.Fatal(e)
	}
	dirBefore, _ := fs.Stat(task.NS(), base+"tmpfile_dir")
	for _, excl := range []bool{false, true} {
		for _, trunc := range []bool{false, true} {
			code := "EINVAL"
			if excl {
				code = "EEXIST"
			}
			check(call("tmpfile_dir", "write", true, excl, trunc, 0777), code)
		}
	}
	dirAfter, _ := fs.Stat(task.NS(), base+"tmpfile_dir")
	if dirBefore.Sys() != dirAfter.Sys() || dirBefore.Mode() != dirAfter.Mode() || !dirBefore.ModTime().Equal(dirAfter.ModTime()) {
		t.Fatal("directory mutated")
	}
	task.Exit("0")
	check(call("tmpfile_exit", "write", true, false, false, 0600), "EIO")
	if _, e := fs.Stat(task.NS(), base+"tmpfile_exit"); e == nil {
		t.Fatal("exit created")
	}
}
