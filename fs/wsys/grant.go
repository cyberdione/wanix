package wsys

import (
	"context"
	"strings"

	"tractor.dev/wanix/fs"
)

// grant is the task-side view of a bound wsys device or window. The default
// grant exposes exactly the rio window files and denies the srv plane at the
// device; the WM grant (bind source "#wsys/wm") additionally admits srv —
// the srv/state enumeration and every window's srv operations — for the one
// task the driver binds it into. The device object itself (root namespace,
// the display server) is ungranted and admits srv, unchanged from N10.
//
// The grant is a plain delegating FS on purpose: it sees every name
// mount-relative and whole, so a srv path cannot escape the check by
// routing straight into a window (the device's Route hands <id>/rest to the
// window, which has no grant of its own).
type grant struct {
	fsys fs.FS // *Device or *window
	wm   bool
}

func (g grant) Stat(name string) (fs.FileInfo, error) {
	return g.StatContext(context.Background(), name)
}

func (g grant) Open(name string) (fs.File, error) {
	return g.OpenContext(context.Background(), name)
}

func (g grant) StatContext(ctx context.Context, name string) (fs.FileInfo, error) {
	if !g.wm && srvName(name) {
		return nil, fs.ErrPermission
	}
	return fs.StatContext(ctx, g.fsys, name)
}

func (g grant) OpenContext(ctx context.Context, name string) (fs.File, error) {
	if !g.wm && srvName(name) {
		return nil, fs.ErrPermission
	}
	return fs.OpenContext(ctx, g.fsys, name)
}

// srvName reports whether a device-local name reaches the host service
// plane: the device-level enumeration (#wsys/srv/state) or any window's
// srv directory. Names arrive mount-relative ("1/srv/consin"); the "wsys/"
// prefix is accepted because the device also serves bound as "#wsys".
func srvName(name string) bool {
	name = strings.TrimPrefix(name, "wsys/")
	if name == "srv" || strings.HasPrefix(name, "srv/") {
		return true
	}
	_, rest, ok := splitID(name)
	return ok && (rest == "srv" || strings.HasPrefix(rest, "srv/"))
}
