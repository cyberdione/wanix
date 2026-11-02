package api

import (
	"io"
	"log"

	"tractor.dev/toolkit-go/duplex/rpc"
)

func (s *syscaller) read(r rpc.Responder, c *rpc.Call) {
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

	count, ok := args[1].(uint64)
	if !ok {
		panic("arg 1 is not a uint64")
	}

	buf := make([]byte, count)
	log.Printf("KDBG2 Read: task=%s fd=%d count=%d", s.task.ID(), fd, count) // KDBG2 (temporary live-pass instrumentation)
	n, err := f.Read(buf)
	if err == io.EOF {
		log.Printf("KDBG2 Read: fd=%d -> n=%d EOF -> nil", fd, n) // KDBG2 (temporary)
		r.Return(nil)
		return
	}
	if err != nil {
		log.Printf("KDBG2 Read: fd=%d -> err=%v", fd, err) // KDBG2 (temporary)
		r.Return(err)
		return
	}

	log.Printf("KDBG2 Read: fd=%d -> n=%d", fd, n) // KDBG2 (temporary)
	r.Return(buf[:n])
}
