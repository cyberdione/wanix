// Package scratchfs owns an in-memory hierarchical scratch namespace and live
// inode storage. It does not wrap memfs. Symlinks are deliberately unsupported.
package scratchfs

import (
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// FS serializes namespace, inode data and metadata with mu. Handles lock their
// own state before mu; filesystem operations never acquire a handle lock.
type FS struct {
	mu    sync.Mutex
	nodes map[string]*inode
	next  uint64
}
type inode struct {
	id    uint64
	data  []byte
	mode  fs.FileMode
	mtime time.Time
}

// Identity is a stable, filesystem-local inode number returned by FileInfo.Sys.
type Identity struct{ Ino uint64 }
type info struct {
	name  string
	size  int64
	mode  fs.FileMode
	mtime time.Time
	id    uint64
}

func (i info) Name() string       { return path.Base(i.name) }
func (i info) Size() int64        { return i.size }
func (i info) Mode() fs.FileMode  { return i.mode }
func (i info) ModTime() time.Time { return i.mtime }
func (i info) IsDir() bool        { return i.mode.IsDir() }
func (i info) Sys() any           { return Identity{i.id} }
func New() *FS {
	f := &FS{nodes: make(map[string]*inode)}
	f.nodes["."] = f.newNode(fs.ModeDir | 0755)
	return f
}
func (f *FS) newNode(mode fs.FileMode) *inode {
	f.next++
	return &inode{id: f.next, mode: mode, mtime: time.Now()}
}
func perr(op, n string, e error) error { return &fs.PathError{Op: op, Path: n, Err: e} }
func (f *FS) lookup(n string) (*inode, error) {
	if !fs.ValidPath(n) {
		return nil, fs.ErrInvalid
	}
	v := f.nodes[n]
	if v == nil {
		return nil, fs.ErrNotExist
	}
	return v, nil
}
func (f *FS) parent(n string) error {
	v, e := f.lookup(path.Dir(n))
	if e != nil {
		return e
	}
	if !v.mode.IsDir() {
		return fs.ErrInvalid
	}
	return nil
}
func (f *FS) snapshot(n string, v *inode) info {
	s := int64(len(v.data))
	if v.mode.IsDir() {
		s = 2
		// Count children of the retained directory inode, not the handle's
		// original pathname: rename or recreation must not change its identity.
		for k := range f.nodes {
			if k != "." && f.nodes[path.Dir(k)] == v {
				s++
			}
		}
	}
	return info{n, s, v.mode, v.mtime, v.id}
}
func (f *FS) Stat(n string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, e := f.lookup(n)
	if e != nil {
		return nil, perr("stat", n, e)
	}
	return f.snapshot(n, v), nil
}

// Open retains the historical scratch read/write access; explicit OpenFile
// handles instead enforce their immutable access mode.
func (f *FS) Open(n string) (fs.File, error) { return f.open(n, os.O_RDWR, 0, true, true, true) }
func (f *FS) Create(n string) (fs.File, error) {
	return f.OpenFile(n, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0644)
}

// WriteFile uses native storage and applies perm only when creating a new inode.
func (f *FS) WriteFile(n string, data []byte, perm fs.FileMode) error {
	h, e := f.OpenHandle(n, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if e != nil {
		return e
	}
	_, e = h.Write(data)
	ce := h.Close()
	if e != nil {
		return e
	}
	return ce
}

func (f *FS) OpenFile(n string, flags int, perm fs.FileMode) (fs.File, error) {
	return f.OpenHandle(n, flags, perm)
}

// OpenHandle is the typed form of OpenFile.
func (f *FS) OpenHandle(n string, flags int, perm fs.FileMode) (*File, error) {
	return f.open(n, flags, perm, true, true, false)
}

// OpenFileGranted bounds access independently of mode bits. Creation requires a
// write grant even when the resulting handle is read-only.
func (f *FS) OpenFileGranted(n string, flags int, perm fs.FileMode, read, write bool) (*File, error) {
	return f.open(n, flags, perm, read, write, false)
}
func (f *FS) open(n string, flags int, perm fs.FileMode, read, write, legacy bool) (*File, error) {
	fail := func(e error) (*File, error) { return nil, perr("open", n, e) }
	access := flags & 3
	rd := access != os.O_WRONLY
	wr := access != os.O_RDONLY
	if !fs.ValidPath(n) || access == 3 || flags & ^(3|os.O_CREATE|os.O_EXCL|os.O_TRUNC|os.O_APPEND) != 0 || perm & ^fs.FileMode(0777) != 0 || flags&os.O_TRUNC != 0 && !wr || flags&os.O_APPEND != 0 && !wr {
		return fail(fs.ErrInvalid)
	}
	if rd && !read || wr && !write || flags&(os.O_CREATE|os.O_TRUNC) != 0 && !write {
		return fail(fs.ErrPermission)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.nodes[n]
	if v != nil && flags&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL {
		return fail(fs.ErrExist)
	}
	if v == nil {
		if flags&os.O_CREATE == 0 {
			return fail(fs.ErrNotExist)
		}
		if e := f.parent(n); e != nil {
			return fail(e)
		}
		v = f.newNode(perm)
		f.nodes[n] = v
	}
	if v.mode.IsDir() && !legacy {
		if wr || flags&(os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
			return fail(fs.ErrInvalid)
		}
	}
	if flags&os.O_TRUNC != 0 {
		v.data = nil
		v.mtime = time.Now()
	}
	h := &File{fs: f, node: v, name: n, read: rd, write: wr, append: flags&os.O_APPEND != 0}
	if v.mode.IsDir() {
		for k, c := range f.nodes {
			if k != "." && path.Dir(k) == n {
				h.entries = append(h.entries, fs.FileInfoToDirEntry(f.snapshot(k, c)))
			}
		}
		sort.Slice(h.entries, func(i, j int) bool { return h.entries[i].Name() < h.entries[j].Name() })
	}
	return h, nil
}
func (f *FS) Mkdir(n string, perm fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !fs.ValidPath(n) || perm & ^fs.FileMode(0777) != 0 {
		return perr("mkdir", n, fs.ErrInvalid)
	}
	if f.nodes[n] != nil {
		return perr("mkdir", n, fs.ErrExist)
	}
	if e := f.parent(n); e != nil {
		return perr("mkdir", n, e)
	}
	f.nodes[n] = f.newNode(fs.ModeDir | perm)
	return nil
}
func (f *FS) Remove(n string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, e := f.lookup(n)
	if e != nil {
		return perr("remove", n, e)
	}
	if n == "." {
		return perr("remove", n, fs.ErrInvalid)
	}
	if v.mode.IsDir() {
		for k := range f.nodes {
			if strings.HasPrefix(k, n+"/") {
				return perr("remove", n, fs.ErrInvalid)
			}
		}
	}
	delete(f.nodes, n)
	return nil
}
func (f *FS) Rename(old, n string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fail := func(e error) error { return perr("rename", old, e) }
	v, e := f.lookup(old)
	if e != nil {
		return fail(e)
	}
	if !fs.ValidPath(n) || old == "." || n == "." {
		return fail(fs.ErrInvalid)
	}
	if old == n {
		return nil
	}
	if strings.HasPrefix(n, old+"/") {
		return fail(fs.ErrInvalid)
	}
	if e = f.parent(n); e != nil {
		return fail(e)
	}
	if dst := f.nodes[n]; dst != nil {
		if dst.mode.IsDir() != v.mode.IsDir() {
			return fail(fs.ErrInvalid)
		}
		if dst.mode.IsDir() {
			for k := range f.nodes {
				if strings.HasPrefix(k, n+"/") {
					return fail(fs.ErrInvalid)
				}
			}
		}
	}
	moved := map[string]*inode{}
	for k, c := range f.nodes {
		if k == old || strings.HasPrefix(k, old+"/") {
			moved[n+strings.TrimPrefix(k, old)] = c
			delete(f.nodes, k)
		}
	}
	for k, c := range moved {
		f.nodes[k] = c
	}
	return nil
}
func (f *FS) Chmod(n string, mode fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, e := f.lookup(n)
	if e != nil {
		return perr("chmod", n, e)
	}
	if mode & ^fs.FileMode(0777) != 0 {
		return perr("chmod", n, fs.ErrInvalid)
	}
	v.mode = v.mode.Type() | mode
	return nil
}
func (f *FS) Chtimes(n string, atime, mtime time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, e := f.lookup(n)
	if e != nil {
		return perr("chtimes", n, e)
	}
	v.mtime = mtime
	return nil
}
func (f *FS) Chown(n string, uid, gid int) error { return perr("chown", n, fs.ErrInvalid) }
func (f *FS) Symlink(target, n string) error     { return perr("symlink", n, fs.ErrInvalid) }
func (f *FS) Readlink(n string) (string, error)  { return "", perr("readlink", n, fs.ErrInvalid) }
func (f *FS) Truncate(n string, size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, e := f.lookup(n)
	if e != nil {
		return perr("truncate", n, e)
	}
	return resize(v, size)
}
func validSize(n int64) bool { return n >= 0 && uint64(n) <= uint64(int(^uint(0)>>1)) }
func resize(v *inode, n int64) error {
	if !validSize(n) || v.mode.IsDir() {
		return fs.ErrInvalid
	}
	if n <= int64(len(v.data)) {
		clear(v.data[n:])
		v.data = v.data[:n]
	} else {
		v.data = append(v.data, make([]byte, int(n)-len(v.data))...)
	}
	v.mtime = time.Now()
	return nil
}

type File struct {
	mu                          sync.Mutex
	fs                          *FS
	node                        *inode
	name                        string
	off                         int64
	closed, read, write, append bool
	entries                     []fs.DirEntry
	dirOff                      int
}

func (h *File) lock()   { h.mu.Lock(); h.fs.mu.Lock() }
func (h *File) unlock() { h.fs.mu.Unlock(); h.mu.Unlock() }
func (h *File) check(write bool) error {
	if h.closed {
		return fs.ErrClosed
	}
	if write && !h.write || !write && !h.read {
		return fs.ErrPermission
	}
	if h.node.mode.IsDir() {
		return fs.ErrInvalid
	}
	return nil
}
func (h *File) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return fs.ErrClosed
	}
	h.closed = true
	return nil
}
func (h *File) Stat() (fs.FileInfo, error) {
	h.lock()
	defer h.unlock()
	if h.closed {
		return nil, fs.ErrClosed
	}
	return h.fs.snapshot(h.name, h.node), nil
}
func (h *File) readAt(b []byte, off int64) (int, error) {
	if e := h.check(false); e != nil {
		return 0, e
	}
	if off < 0 {
		return 0, fs.ErrInvalid
	}
	if len(b) == 0 {
		return 0, nil
	}
	if off >= int64(len(h.node.data)) {
		return 0, io.EOF
	}
	n := copy(b, h.node.data[off:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}
func (h *File) Read(b []byte) (int, error) {
	h.lock()
	defer h.unlock()
	n, e := h.readAt(b, h.off)
	h.off += int64(n)
	return n, e
}
func (h *File) ReadAt(b []byte, off int64) (int, error) {
	h.lock()
	defer h.unlock()
	return h.readAt(b, off)
}
func (h *File) writeAt(b []byte, off int64) (int, error) {
	if e := h.check(true); e != nil {
		return 0, e
	}
	if !validSize(off) || int64(len(b)) > int64(int(^uint(0)>>1))-off {
		return 0, fs.ErrInvalid
	}
	if len(b) == 0 {
		return 0, nil
	}
	end := off + int64(len(b))
	if end > int64(len(h.node.data)) {
		if e := resize(h.node, end); e != nil {
			return 0, e
		}
	}
	copy(h.node.data[off:], b)
	h.node.mtime = time.Now()
	return len(b), nil
}
func (h *File) Write(b []byte) (int, error) {
	h.lock()
	defer h.unlock()
	off := h.off
	if h.append {
		off = int64(len(h.node.data))
	}
	n, e := h.writeAt(b, off)
	if e == nil {
		h.off = off + int64(n)
	}
	return n, e
}
func (h *File) WriteAt(b []byte, off int64) (int, error) {
	h.lock()
	defer h.unlock()
	if e := h.check(true); e != nil {
		return 0, e
	}
	if h.append {
		return 0, fs.ErrInvalid
	}
	return h.writeAt(b, off)
}
func (h *File) Seek(off int64, whence int) (int64, error) {
	h.lock()
	defer h.unlock()
	if h.closed {
		return 0, fs.ErrClosed
	}
	if h.node.mode.IsDir() {
		return 0, fs.ErrInvalid
	}
	base := int64(0)
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = h.off
	case io.SeekEnd:
		base = int64(len(h.node.data))
	default:
		return 0, fs.ErrInvalid
	}
	if off > 0 && base > int64(^uint64(0)>>1)-off || off < 0 && off < -base {
		return 0, fs.ErrInvalid
	}
	h.off = base + off
	return h.off, nil
}
func (h *File) Truncate(n int64) error {
	h.lock()
	defer h.unlock()
	if e := h.check(true); e != nil {
		return e
	}
	return resize(h.node, n)
}
func (h *File) Sync() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return fs.ErrClosed
	}
	return fs.ErrInvalid
}
func (h *File) ReadDir(n int) ([]fs.DirEntry, error) {
	h.lock()
	defer h.unlock()
	if h.closed {
		return nil, fs.ErrClosed
	}
	if !h.read {
		return nil, fs.ErrPermission
	}
	if !h.node.mode.IsDir() {
		return nil, fs.ErrInvalid
	}
	if n > 0 && h.dirOff == len(h.entries) {
		return nil, io.EOF
	}
	end := len(h.entries)
	if n > 0 && n < end-h.dirOff {
		end = h.dirOff + n
	}
	out := append([]fs.DirEntry{}, h.entries[h.dirOff:end]...)
	h.dirOff = end
	return out, nil
}
