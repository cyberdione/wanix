package api

import (
	"syscall"
	"tractor.dev/toolkit-go/duplex/rpc"
)

// openFileResult accepts semantic fields, never guest numeric flags. Creation is
// deliberately unavailable until the personality has an explicit mask contract.
func (s *syscaller) openFileResult(r rpc.Responder, c *rpc.Call) {
	var req map[string]any
	if err := c.Receive(&req); err != nil {
		_ = r.Return(failedFilesystemResult[uint64](syscall.EINVAL))
		return
	}
	fail := func(e syscall.Errno) { _ = r.Return(failedFilesystemResult[uint64](e)) }
	if len(req) != 6 {
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
	if create || excl {
		fail(syscall.ENOSYS)
		return
	}
	f, err := s.task.OpenScratchFile(name, access, trunc)
	if err != nil {
		_ = r.Return(failedFilesystemResult[uint64](err))
		return
	}
	s.publishOpenResult(r, f, name)
}
