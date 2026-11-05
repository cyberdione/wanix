//go:build js && wasm

// File System Access API
package fsa

import (
	"errors"
	"path"
	"sync"
	"syscall/js"
	"time"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/misc/jsutil"
)

// user selected directory
func ShowDirectoryPicker() fs.FS {
	dir := jsutil.Await(js.Global().Get("window").Call("showDirectoryPicker"))
	return NewFS(dir)
}

var opfsSingleton *FS
var opfsMu sync.Mutex

// opfsTimeoutMs bounds the OPFS handshake. On opaque origins (e.g. file://)
// navigator.storage.getDirectory() can pend forever; boot must not.
var opfsTimeoutMs = 3000

// awaitBounded awaits a js promise, failing after ms if it never settles.
func awaitBounded(p js.Value, ms int) (js.Value, error) {
	type result struct {
		val js.Value
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := jsutil.AwaitErr(p)
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.val, r.err
	case <-time.After(time.Duration(ms) * time.Millisecond):
		return js.Undefined(), errors.New("opfs handshake timed out")
	}
}

// origin private file system
// OPFS returns a filesystem for the Origin Private File System.
// It can take optional path arguments to return a subdirectory:
//
//	OPFS() - returns root OPFS
//	OPFS("path/to/dir") - returns subdirectory at path
//	OPFS("path", "to", "dir") - returns subdirectory using path parts
func OPFS(pathParts ...string) (fs.FS, error) {
	opfsMu.Lock()
	singleton := opfsSingleton
	opfsMu.Unlock()

	if singleton == nil {
		if js.Global().Get("navigator").Get("storage").IsUndefined() {
			return nil, errors.New("navigator.storage is undefined")
		}
		rootDir, err := awaitBounded(js.Global().Get("navigator").Get("storage").Call("getDirectory"), opfsTimeoutMs)
		if err != nil {
			return nil, err
		}
		fsys := NewFS(rootDir)
		if err := Metadata().Initialize(fsys); err != nil {
			return nil, err
		}
		opfsMu.Lock()
		opfsSingleton = fsys
		opfsMu.Unlock()
	}

	if len(pathParts) == 0 {
		return opfsSingleton, nil
	}

	dir := path.Join(pathParts...)
	if err := fs.MkdirAll(opfsSingleton, dir, 0o755); err != nil {
		return nil, err
	}
	return fs.Sub(opfsSingleton, dir)
}
