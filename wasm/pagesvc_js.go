//go:build js && wasm

package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"syscall/js"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
	"tractor.dev/wanix/misc/jsutil"
	"tractor.dev/wanix/web/jsfs"
)

// pageTCPService adapts the session's page-owned TCP service object
// (globalThis.__ferruleosTcpService) to the net/tcp ABI tasks already use.
//
// The page object carries `new` (a pure getter returning the next id), one
// object per connection exposing ctl (function) plus status/generation
// (getters), and service-level read/write/shutdown functions. Everything
// except the binary data channel is projected through jsfs, so those
// handlers run as page JS. jsfs payloads are strings only, so <id>/data is
// served by pageDataFile, whose Read/Write await promises that the page's
// own pump loops settle. The kernel never calls a provider stream directly:
// every underlying arti read/write originates from the page pumps
// (kernel-initiated provider writes are accepted but never reach the wire;
// kernel-initiated reads work but are pump-fed here anyway).
type pageTCPService struct {
	svc js.Value
	js  *jsfs.FS
}

func newPageTCPService(svc js.Value) *pageTCPService {
	return &pageTCPService{svc: svc, js: jsfs.NewFS(svc)}
}

// pageTCPCapable reports whether the page registered a usable TCP service.
func pageTCPCapable(svc js.Value) bool {
	return svc.Truthy() &&
		svc.Get("read").Type() == js.TypeFunction &&
		svc.Get("write").Type() == js.TypeFunction &&
		svc.Get("new").Type() == js.TypeString
}

func jsstr(v js.Value) string {
	if v.Type() == js.TypeString {
		return v.String()
	}
	return js.Global().Get("String").Invoke(v).String()
}

func (p *pageTCPService) Open(name string) (fs.File, error) {
	return p.OpenContext(context.Background(), name)
}

func (p *pageTCPService) OpenContext(ctx context.Context, name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if f, handled, err := p.openData(name); handled {
		return f, err
	}
	return fs.OpenContext(ctx, p.js, name)
}

func (p *pageTCPService) Stat(name string) (fs.FileInfo, error) {
	return p.StatContext(context.Background(), name)
}

func (p *pageTCPService) StatContext(ctx context.Context, name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	if _, handled, err := p.openData(name); handled {
		if err != nil {
			return nil, err
		}
		return fskit.Entry("data", 0644), nil
	}
	return fs.StatContext(ctx, p.js, name)
}

func (p *pageTCPService) Route(_ context.Context, name string) (fs.FS, string, error) {
	if !fs.ValidPath(name) {
		return nil, "", &fs.PathError{Op: "route", Path: name, Err: fs.ErrInvalid}
	}
	return p, name, nil
}

// openData serves <id>/data and <id>/data/<generation>. It reports handled
// when the path names a data file; the connection object and its generation
// are validated against the live page state (pure getter reads).
func (p *pageTCPService) openData(name string) (fs.File, bool, error) {
	if name == "." {
		return nil, false, nil
	}
	parts := strings.Split(name, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[1] != "data" {
		return nil, false, nil
	}
	rid := parts[0]
	conn := p.svc.Get(rid)
	if !conn.Truthy() {
		return nil, true, fs.ErrNotExist
	}
	gen := strings.TrimSpace(jsstr(conn.Get("generation")))
	if len(parts) == 3 {
		req := parts[2]
		parsed, err := strconv.ParseUint(req, 10, 64)
		if err != nil || strconv.FormatUint(parsed, 10) != req {
			return nil, true, fs.ErrNotExist
		}
		if req != gen {
			return nil, true, fs.ErrPermission
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(jsstr(conn.Get("status"))), "connected") {
		return nil, true, fs.ErrPermission
	}
	return &pageDataFile{svc: p.svc, rid: rid, gen: gen}, true, nil
}

// Truncate makes the O_TRUNC artifact of os.WriteFile-style opens on the
// synthetic control files a no-op. The generic fs.Truncate fallback would
// io.ReadAll the jsfs funcfile, which blocks forever waiting for an
// invocation result — that is exactly how a WASI ctl open wedges a task.
func (p *pageTCPService) Truncate(name string, size int64) error {
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: "truncate", Path: name, Err: fs.ErrInvalid}
	}
	if _, handled, err := p.openData(name); handled {
		if err != nil {
			return err
		}
		return nil // byte stream: truncation is meaningless, tolerate
	}
	if _, err := fs.StatContext(context.Background(), p.js, name); err != nil {
		return err // ENOENT and friends still surface
	}
	return nil
}

// CloseAll tears down every page connection when the grant is revoked.
// The page shutdown only flips state; stream closes are issued from the
// page's own microtasks.
func (p *pageTCPService) CloseAll() {
	if shutdown := p.svc.Get("shutdown"); shutdown.Type() == js.TypeFunction {
		shutdown.Invoke()
	}
}

// pageDataFile is the task-side handle for one dial generation's byte
// stream. Read/Write await promises settled by the page service's pump
// loops. Closing the handle only pins it dead; the allocation is released
// with ctl dispose, matching the tinygo service's dataFile semantics.
type pageDataFile struct {
	svc js.Value
	rid string
	gen string

	mu   sync.Mutex
	dead bool
}

var _ fs.File = (*pageDataFile)(nil)

func (f *pageDataFile) Stat() (fs.FileInfo, error) { return fskit.Entry("data", 0644), nil }

func (f *pageDataFile) Close() error {
	f.mu.Lock()
	f.dead = true
	f.mu.Unlock()
	return nil
}

func (f *pageDataFile) live() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.dead
}

func (f *pageDataFile) Read(p []byte) (int, error) {
	if !f.live() {
		return 0, fs.ErrPermission
	}
	if len(p) == 0 {
		return 0, nil
	}
	v, err := jsutil.AwaitErr(f.svc.Call("read", js.ValueOf(f.rid), js.ValueOf(f.gen), js.ValueOf(len(p))))
	if err != nil {
		return 0, err
	}
	if v.IsNull() || v.IsUndefined() {
		return 0, io.EOF
	}
	length := v.Get("byteLength")
	if length.Type() != js.TypeNumber {
		return 0, fmt.Errorf("net read: invalid data")
	}
	n := length.Int()
	if n < 0 || n > len(p) {
		return 0, fmt.Errorf("net read: too much data")
	}
	if n > 0 {
		js.CopyBytesToGo(p[:n], v)
	}
	return n, nil
}

func (f *pageDataFile) Write(p []byte) (int, error) {
	if !f.live() {
		return 0, fs.ErrPermission
	}
	buf := js.Global().Get("Uint8Array").New(len(p))
	js.CopyBytesToJS(buf, p)
	v, err := jsutil.AwaitErr(f.svc.Call("write", js.ValueOf(f.rid), js.ValueOf(f.gen), buf))
	if err != nil {
		return 0, err
	}
	if v.Type() != js.TypeNumber {
		return 0, fmt.Errorf("net write: invalid count")
	}
	n := v.Int()
	if n < 0 || n > len(p) {
		return 0, fmt.Errorf("net write: invalid count")
	}
	return n, nil
}
