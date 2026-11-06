package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"

	"tractor.dev/toolkit-go/duplex/codec"
	"tractor.dev/toolkit-go/duplex/mux"
	"tractor.dev/wanix"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
)

func TestFilesystemCode(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{&fs.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist}, "ENOENT"},
		{fmt.Errorf("wrapped: %w", syscall.EPERM), "EPERM"},
		{syscall.ENOSYS, "EIO"}, {syscall.Errno(0), "EIO"},
		{fs.ErrPermission, "EACCES"}, {fs.ErrExist, "EEXIST"}, {fs.ErrInvalid, "EINVAL"},
		{fs.ErrClosed, "EIO"}, {context.Canceled, "EIO"},
		{errors.New("file does not exist: permission denied"), "EIO"},
		{errors.Join(fs.ErrNotExist, syscall.EPERM), "EPERM"},
	}
	for _, c := range cases {
		if got := filesystemCode(c.err); got != c.code {
			t.Errorf("%v: %s != %s", c.err, got, c.code)
		}
	}
	for _, e := range []syscall.Errno{syscall.ENOENT, syscall.EACCES, syscall.EEXIST, syscall.EINVAL, syscall.ENOTDIR, syscall.EISDIR, syscall.ELOOP, syscall.ENAMETOOLONG, syscall.EIO} {
		if filesystemCode(e) == "EIO" && e != syscall.EIO {
			t.Errorf("not classified: %v", e)
		}
	}
}
func TestFilesystemResultsRPC(t *testing.T) {
	client, task := startTestServerWithTask(t)
	ctx := context.Background()
	if _, err := client.Call(ctx, "WriteFile", []any{"file", []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"OpenResult", "StatResult", "LstatResult"} {
		var missing map[string]any
		resp, err := client.Call(ctx, selector, []string{"missing"}, &missing)
		if err != nil || resp.E != nil {
			t.Fatalf("%s: %v", selector, err)
		}
		if missing["OK"] != false || missing["Value"] != nil || len(missing) != 2 {
			t.Fatalf("failure: %#v", missing)
		}
		if missing["Error"].(map[any]any)["Code"] != "ENOENT" {
			t.Fatal(missing)
		}
		var got map[string]any
		resp, err = client.Call(ctx, selector, []string{"file"}, &got)
		if err != nil || resp.E != nil || got["OK"] != true || len(got) != 2 {
			t.Fatalf("%s: %#v %v", selector, got, err)
		}
		if selector == "OpenResult" {
			fd := got["Value"].(uint64)
			if _, _, err := task.FD(int(fd)); err != nil {
				t.Fatal(err)
			}
			if err := task.CloseFD(int(fd)); err != nil {
				t.Fatal(err)
			}
		} else {
			var old map[string]any
			if _, err := client.Call(ctx, selector[:len(selector)-6], []string{"file"}, &old); err != nil {
				t.Fatal(err)
			}
			v := got["Value"].(map[any]any)
			for k, x := range old {
				if fmt.Sprint(v[k]) != fmt.Sprint(x) {
					t.Fatalf("legacy %s: %#v != %#v", k, v[k], x)
				}
			}
		}
		if _, err := client.Call(ctx, selector, []string{}); err == nil {
			t.Fatal("accepted missing argument")
		}
		if _, err := client.Call(ctx, selector, []any{7}); err == nil {
			t.Fatal("accepted wrong type")
		}
	}
	var old uint64
	if _, err := client.Call(ctx, "Open", []string{"file"}, &old); err != nil {
		t.Fatal(err)
	}
	_ = task.CloseFD(int(old))
	for _, sel := range []string{"Open", "Stat", "Lstat"} {
		if _, err := client.Call(ctx, sel, []string{"missing"}); err == nil {
			t.Fatalf("legacy %s missing error", sel)
		}
	}
	task.Exit("done")
	var late map[string]any
	if _, err := client.Call(ctx, "OpenResult", []string{"file"}, &late); err != nil {
		t.Fatal(err)
	}
	if late["OK"] != false || late["Value"] != nil {
		t.Fatalf("late: %#v", late)
	}
}
func TestFilesystemWireFixture(t *testing.T) {
	zero := uint64(0)
	stat := statValue{Size: -1, Mode: 33188, IsDir: false, ModTime: time.Unix(1700000000, 0).UTC()}
	fixtures := []any{filesystemResult[uint64]{OK: true, Value: &zero}, filesystemResult[statValue]{OK: true, Value: &stat}, failedFilesystemResult[uint64](fs.ErrNotExist)}
	data, err := os.ReadFile("filesystem_result.fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]string
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	names := []string{"openZero", "statNegative", "notExist"}
	for i, v := range fixtures {
		var buf bytes.Buffer
		if err := (codec.CBORCodec{}).Encoder(&buf).Encode(v); err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(buf.Bytes()); got != golden[names[i]] {
			t.Fatalf("%s wire changed: %s", names[i], got)
		}
	}
}

type resultTestFile struct {
	fs.File
	closes int
}

func (f *resultTestFile) Close() error               { f.closes++; return nil }
func (f *resultTestFile) Stat() (fs.FileInfo, error) { return fskit.Entry(".", fs.ModeDir|0755), nil }

type resultTestResponder struct {
	value any
	err   error
}

func (r *resultTestResponder) Return(v ...any) error                { r.value = v[0]; return r.err }
func (r *resultTestResponder) Continue(...any) (mux.Channel, error) { panic("unused") }
func (r *resultTestResponder) Send(any) error                       { panic("unused") }
func TestOpenResultAllocationAndPublicationCleanup(t *testing.T) {
	for _, exited := range []bool{false, true} {
		t.Run(fmt.Sprint(exited), func(t *testing.T) {
			task, err := wanix.NewRoot()
			if err != nil {
				t.Fatal(err)
			}
			file := &resultTestFile{}
			if exited {
				task.Exit("done")
			}
			responder := &resultTestResponder{err: errors.New("publication failed")}
			(&syscaller{task: task}).publishOpenResult(responder, file, "file")
			got := responder.value.(filesystemResult[uint64])
			if exited {
				if got.OK || got.Value != nil || got.Error.Code != "EIO" {
					t.Fatalf("%#v", got)
				}
			} else {
				if !got.OK {
					t.Fatal(got)
				}
				if _, _, err := task.FD(int(*got.Value)); err == nil {
					t.Fatal("unpublished fd retained")
				}
			}
			if file.closes != 1 {
				t.Fatalf("closed %d times", file.closes)
			}
		})
	}
}
