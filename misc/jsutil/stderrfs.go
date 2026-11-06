//go:build js && wasm

package jsutil

import (
	"bytes"
	"context"
	iofs "io/fs"
	"os"
	"sync"
	"syscall/js"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
)

// StderrFS is a stream endpoint that routes fd 2 lines to console.error.
var StderrFS fs.FS = &stderrFS{w: new(stderrWriter)}

type stderrFS struct {
	mu sync.Mutex
	w  *stderrWriter
	n  int
}

func (c *stderrFS) Route(ctx context.Context, name string) (fs.FS, string, error) {
	if name == "." {
		return c, ".", nil
	}
	return nil, "", &fs.PathError{Op: "route", Path: name, Err: fs.ErrNotExist}
}
func (c *stderrFS) Open(name string) (iofs.File, error) {
	return c.OpenContext(context.Background(), name)
}
func (c *stderrFS) OpenContext(ctx context.Context, name string) (fs.File, error) {
	return c.OpenFile(name, os.O_RDONLY, 0)
}
func (c *stderrFS) OpenFile(name string, flag int, perm fs.FileMode) (fs.File, error) {
	if name != "." {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return &stderrHandle{c}, nil
}

type stderrHandle struct{ c *stderrFS }

func (h *stderrHandle) Read([]byte) (int, error)               { return 0, fs.ErrPermission }
func (h *stderrHandle) Write(b []byte) (int, error)            { return h.c.w.Write(b) }
func (h *stderrHandle) WriteAt(b []byte, _ int64) (int, error) { return h.Write(b) }
func (h *stderrHandle) Close() error {
	h.c.mu.Lock()
	h.c.n--
	last := h.c.n == 0
	h.c.mu.Unlock()
	if last {
		h.c.w.flush()
	}
	return nil
}
func (h *stderrHandle) Stat() (fs.FileInfo, error) { return fskit.Entry("stderr", 0644), nil }

type stderrWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (w *stderrWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		js.Global().Get("console").Call("error", string(line))
		w.buf = w.buf[i+1:]
	}
	return len(b), nil
}
func (w *stderrWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) == 0 {
		return
	}
	line := w.buf
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	js.Global().Get("console").Call("error", string(line))
	w.buf = nil
}
