package api

import (
	"context"
	"fmt"
	"io"

	"tractor.dev/toolkit-go/duplex/rpc"
)

func (s *syscaller) read(r rpc.Responder, c *rpc.Call) {
	var args []any
	c.Receive(&args)

	fd, ok := args[0].(uint64)
	if !ok {
		panic(fmt.Sprintf("arg 0 is not a uint64: %T %v", args[1], args[1]))
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
	var n int
	if reader, ok := f.(interface {
		ReadContext(context.Context, []byte) (int, error)
	}); ok {
		n, err = reader.ReadContext(s.task.Context(), buf)
	} else {
		n, err = f.Read(buf)
	}
	if err == io.EOF && n == 0 {
		r.Return(nil)
		return
	}
	if err != nil && err != io.EOF {
		r.Return(err)
		return
	}

	r.Return(buf[:n])
}
