package api

import (
	"log"

	"tractor.dev/toolkit-go/duplex/rpc"
	"tractor.dev/wanix/fs"
)

func (s *syscaller) open(r rpc.Responder, c *rpc.Call) {
	var args []string
	c.Receive(&args)

	f, err := s.task.NS().Open(args[0])
	if err != nil {
		log.Printf("KDBG2 Open: task=%s path=%q -> err=%v", s.task.ID(), args[0], err) // KDBG2 (temporary live-pass instrumentation)
		r.Return(err)
		return
	}

	fd := s.task.OpenFD(f, args[0])
	log.Printf("KDBG2 Open: task=%s path=%q -> fd=%d", s.task.ID(), args[0], fd) // KDBG2 (temporary)
	r.Return(uint64(fd))
}

func (s *syscaller) create(r rpc.Responder, c *rpc.Call) {
	var args []string
	c.Receive(&args)

	f, err := fs.Create(s.task.NS(), args[0])
	if err != nil {
		r.Return(err)
		return
	}

	fd := s.task.OpenFD(f, args[0])
	r.Return(uint64(fd))
}

func (s *syscaller) openFile(r rpc.Responder, c *rpc.Call) {
	var args []any
	c.Receive(&args)

	path, ok := args[0].(string)
	if !ok {
		panic("arg 0 is not a string")
	}

	flags, ok := args[1].(uint64)
	if !ok {
		panic("arg 1 is not a uint64")
	}

	mode, ok := args[2].(uint64)
	if !ok {
		panic("arg 2 is not a uint64")
	}

	f, err := fs.OpenFile(s.task.NS(), path, int(flags), fs.FileMode(mode))
	if err != nil {
		log.Printf("KDBG2 OpenFile: task=%s path=%q -> err=%v", s.task.ID(), path, err) // KDBG2 (temporary)
		r.Return(err)
		return
	}

	fd := s.task.OpenFD(f, path)
	log.Printf("KDBG2 OpenFile: task=%s path=%q -> fd=%d", s.task.ID(), path, fd) // KDBG2 (temporary)
	r.Return(uint64(fd))
}
