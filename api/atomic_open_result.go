package api

import (
	"syscall"
	"tractor.dev/toolkit-go/duplex/rpc"
	"tractor.dev/wanix/fs"
)

// openFileResult accepts semantic fields and already-masked ordinary mode metadata,
// never guest numeric flags or a second umask policy.
func (s *syscaller) openFileResult(r rpc.Responder, c *rpc.Call) {
	var req map[string]any
	if err := c.Receive(&req); err != nil {
		_ = r.Return(failedFilesystemResult[uint64](syscall.EINVAL))
		return
	}
	fail := func(e syscall.Errno) { _ = r.Return(failedFilesystemResult[uint64](e)) }
	if len(req) != 6 && len(req) != 7 {
		fail(syscall.EINVAL)
		return
	}
	name, p := req["Path"].(string)
	access, a := req["Access"].(string)
	create, cr := req["Create"].(bool)
	excl, ex := req["Exclusive"].(bool)
	trunc, tr := req["Truncate"].(bool)
	perm, pe := req["Perm"].(uint64)
	if !p || !a || !cr || !ex || !tr || !pe || perm > 0777 || !create && perm != 0 || excl && !create || access != "read" && access != "write" && access != "readwrite" || access == "read" && trunc {
		fail(syscall.EINVAL)
		return
	}
	var fd int
	var err error
	if raw, present := req["MaxExistingSize"]; present {
		ceiling, valid := raw.(uint64)
		if !valid || ceiling > 9007199254740991 {
			fail(syscall.EINVAL)
			return
		}
		fd, err = s.task.OpenScratchCreateBoundedFD(name, access, trunc, create, excl, fs.FileMode(perm), ceiling)
	} else {
		if len(req) != 6 {
			fail(syscall.EINVAL)
			return
		}
		fd, err = s.task.OpenScratchCreateFD(name, access, trunc, create, excl, fs.FileMode(perm))
	}
	if err != nil {
		_ = r.Return(failedFilesystemResult[uint64](err))
		return
	}
	value := uint64(fd)
	if err := r.Return(filesystemResult[uint64]{OK: true, Value: &value}); err != nil {
		_ = s.task.CloseFD(fd)
	}
}
