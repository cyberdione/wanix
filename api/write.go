package api

import (
	"log"

	"tractor.dev/toolkit-go/duplex/rpc"
	"tractor.dev/wanix/fs"
)

func (s *syscaller) write(r rpc.Responder, c *rpc.Call) {
	var args []any
	c.Receive(&args)

	fd, ok := args[0].(uint64)
	if !ok {
		log.Panicf("arg 0 is not a uint64: %T %v", args[1], args[1])
	}

	f, _, err := s.task.FD(int(fd))
	if err != nil {
		r.Return(err)
		return
	}

	data, ok := args[1].([]byte)
	if !ok {
		panic("arg 1 is not a []byte")
	}

	log.Printf("KDBG2 Write: task=%s fd=%d len=%d", s.task.ID(), fd, len(data)) // KDBG2 (temporary live-pass instrumentation)
	n, err := fs.Write(f, data)
	if err != nil {
		log.Printf("KDBG2 Write: fd=%d -> err=%v", fd, err) // KDBG2 (temporary)
		r.Return(err)
		return
	}

	log.Printf("KDBG2 Write: fd=%d -> n=%d", fd, n) // KDBG2 (temporary)
	r.Return(uint64(n))
}

func (s *syscaller) writeAt(r rpc.Responder, c *rpc.Call) {
	var args []any
	c.Receive(&args)

	fd, ok := args[0].(uint64)
	if !ok {
		log.Panicf("arg 0 is not a uint64: %T %v", args[1], args[1])
	}

	f, _, err := s.task.FD(int(fd))
	if err != nil {
		r.Return(err)
		return
	}

	data, ok := args[1].([]byte)
	if !ok {
		panic("arg 1 is not a []byte")
	}

	offset, ok := args[2].(uint64)
	if !ok {
		panic("arg 2 is not a uint64")
	}

	log.Printf("KDBG2 WriteAt: task=%s fd=%d len=%d off=%d", s.task.ID(), fd, len(data), offset) // KDBG2 (temporary live-pass instrumentation)
	n, err := fs.WriteAt(f, data, int64(offset))
	if err != nil {
		log.Printf("KDBG2 WriteAt: fd=%d -> err=%v", fd, err) // KDBG2 (temporary)
		r.Return(err)
		return
	}

	log.Printf("KDBG2 WriteAt: fd=%d -> n=%d", fd, n) // KDBG2 (temporary)
	r.Return(n)
}
