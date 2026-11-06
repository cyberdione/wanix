package wanix

import (
	"context"
	"os"
	"strings"
	"syscall"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/scratchfs"
)

// OpenScratchFile opens only an existing regular leaf in this task's owned scratch.
// The wire path is not authority: the captured final owner and leaf must both match.
// Exit does not revoke the returned caller-owned handle. Task-owned callers must
// use OpenScratchFD, not a later OpenFD registration after a destructive open.
func (t *Task) OpenScratchFile(name, access string, truncate bool) (fs.File, error) {
	h, _, err := t.openScratchFile(name, access, truncate, false, false, 0, false)
	return h, err
}

// OpenScratchFD admits the backend open and task-owned FD publication together.
// An Exit after success may close the FD, but cannot retroactively reject the open.
func (t *Task) OpenScratchFD(name, access string, truncate bool) (int, error) {
	_, fd, err := t.openScratchFile(name, access, truncate, false, false, 0, true)
	if err != nil {
		return -1, err
	}
	return fd, nil
}

// OpenScratchCreateFD uses already-masked ordinary permission metadata. It does
// not enforce Unix DAC. Creation and descriptor admission share the exit gate.
func (t *Task) OpenScratchCreateFD(name, access string, truncate, create, exclusive bool, perm fs.FileMode) (int, error) {
	_, fd, err := t.openScratchFile(name, access, truncate, create, exclusive, perm, true)
	return fd, err
}

func (t *Task) openScratchFile(name, access string, truncate, create, exclusive bool, perm fs.FileMode, publish bool) (fs.File, int, error) {
	if perm & ^fs.FileMode(0777) != 0 || !create && perm != 0 || exclusive && !create {
		return nil, -1, syscall.EINVAL
	}
	flags := 0
	switch access {
	case "read":
		if truncate {
			return nil, -1, syscall.EINVAL
		}
		flags = os.O_RDONLY
	case "write":
		flags = os.O_WRONLY
	case "readwrite":
		flags = os.O_RDWR
	default:
		return nil, -1, syscall.EINVAL
	}
	if truncate {
		flags |= os.O_TRUNC
	}
	if create {
		flags |= os.O_CREATE
	}
	if exclusive {
		flags |= os.O_EXCL
	}
	prefix := "#task/" + t.ID() + "/w/"
	if !strings.HasPrefix(name, prefix) {
		return nil, -1, syscall.EACCES
	}
	leaf := strings.TrimPrefix(name, prefix)
	if strings.ContainsAny(leaf, "/\x00") || !(leaf == "out.txt" || strings.HasPrefix(leaf, "tmpfile_") && len(leaf) > len("tmpfile_")) {
		return nil, -1, syscall.EACCES
	}
	// Start fresh, retaining task identity but never an inherited read-only operation.
	ctx := context.WithValue(context.Background(), TaskContextKey, t)
	ctx = fs.WithOrigin(ctx, t.NS(), name, "atomic-open")
	if access == "read" && !create {
		ctx = fs.WithReadOnly(ctx)
	}
	// Parent visibility is an additional fail-closed policy, not a fallback
	// authority source. These walks are separate routing snapshots; they do not
	// promise atomic bind-table visibility or revocation on a concurrent swap.
	parent, err := fs.Walk(ctx, t.NS(), strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return nil, -1, syscall.ENOSYS
	}
	if parent.FS != t.w || parent.Rel != "." {
		return nil, -1, syscall.EACCES
	}
	loc, err := fs.Walk(ctx, t.NS(), name)
	if err != nil {
		return nil, -1, err
	}
	if loc.FS != t.w || loc.Rel != leaf {
		return nil, -1, syscall.EACCES
	}
	owned, ok := t.w.(*scratchfs.FS)
	if !ok {
		return nil, -1, syscall.ENOSYS
	}
	// Only concrete owned scratch operations run under mu: no namespace routing,
	// user callbacks, or arbitrary fs.File methods may enter this section.
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exited {
		return nil, -1, fs.ErrClosed
	}
	h, err := owned.OpenFileGranted(leaf, flags, perm, true, true)
	if err != nil {
		return nil, -1, err
	}
	info, err := h.Stat()
	if err != nil {
		h.Close()
		return nil, -1, err
	}
	if info.IsDir() {
		h.Close()
		return nil, -1, syscall.EISDIR
	}
	if publish {
		t.fdIdx++
		t.fds[t.fdIdx] = &openFile{file: h, path: name}
		return nil, t.fdIdx, nil
	}
	return h, -1, nil
}
