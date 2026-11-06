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
func (t *Task) OpenScratchFile(name, access string, truncate bool) (fs.File, error) {
	flags := 0
	switch access {
	case "read":
		if truncate {
			return nil, syscall.EINVAL
		}
		flags = os.O_RDONLY
	case "write":
		flags = os.O_WRONLY
	case "readwrite":
		flags = os.O_RDWR
	default:
		return nil, syscall.EINVAL
	}
	if truncate {
		flags |= os.O_TRUNC
	}
	prefix := "#task/" + t.ID() + "/w/"
	if !strings.HasPrefix(name, prefix) {
		return nil, syscall.EACCES
	}
	leaf := strings.TrimPrefix(name, prefix)
	if strings.ContainsAny(leaf, "/\x00") || !(leaf == "out.txt" || strings.HasPrefix(leaf, "tmpfile_") && len(leaf) > len("tmpfile_")) {
		return nil, syscall.EACCES
	}
	// Start fresh, retaining task identity but never an inherited read-only operation.
	ctx := context.WithValue(context.Background(), TaskContextKey, t)
	ctx = fs.WithOrigin(ctx, t.NS(), name, "atomic-open")
	if access == "read" {
		ctx = fs.WithReadOnly(ctx)
	}
	loc, err := fs.Walk(ctx, t.NS(), name)
	if err != nil {
		return nil, err
	}
	if loc.FS != t.w || loc.Rel != leaf {
		return nil, syscall.EACCES
	}
	owned, ok := t.w.(*scratchfs.FS)
	if !ok {
		return nil, syscall.ENOSYS
	}
	h, err := owned.OpenFileGranted(leaf, flags, 0, true, true)
	if err != nil {
		return nil, err
	}
	info, err := h.Stat()
	if err != nil {
		h.Close()
		return nil, err
	}
	if info.IsDir() {
		h.Close()
		return nil, syscall.EISDIR
	}
	return h, nil
}
