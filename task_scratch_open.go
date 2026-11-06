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
	h, _, err := t.openScratchFile(name, access, truncate, false)
	return h, err
}

// OpenScratchFD admits the backend open and task-owned FD publication together.
// An Exit after success may close the FD, but cannot retroactively reject the open.
func (t *Task) OpenScratchFD(name, access string, truncate bool) (int, error) {
	_, fd, err := t.openScratchFile(name, access, truncate, true)
	if err != nil {
		return -1, err
	}
	return fd, nil
}

func (t *Task) openScratchFile(name, access string, truncate, publish bool) (fs.File, int, error) {
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
	if access == "read" {
		ctx = fs.WithReadOnly(ctx)
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
	h, err := owned.OpenFileGranted(leaf, flags, 0, true, true)
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
