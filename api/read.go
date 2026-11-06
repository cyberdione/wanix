package api

import (
	"context"
	"fmt"
	"io"
	"math"

	"tractor.dev/toolkit-go/duplex/rpc"
	"tractor.dev/wanix/fs"
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

// readAt requires ReaderAt for positioned reads: seeking and restoring cannot
// preserve the cursor against concurrent sequential reads. Non-seekable streams
// still read sequentially; their offset is only caller bookkeeping.
func (s *syscaller) readAt(r rpc.Responder, c *rpc.Call) {
	var args []any
	c.Receive(&args)
	fd, ok := args[0].(uint64)
	if !ok {
		panic("arg 0 is not a uint64")
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
	offset, ok := args[2].(uint64)
	if !ok {
		panic("arg 2 is not a uint64")
	}
	if offset > math.MaxInt64 {
		r.Return(fmt.Errorf("ReadAt offset exceeds MaxInt64: %d", offset))
		return
	}
	buf := make([]byte, count)
	var n int
	if reader, ok := f.(io.ReaderAt); ok {
		n, err = reader.ReadAt(buf, int64(offset))
	} else if _, seekable := f.(io.Seeker); seekable {
		r.Return(fmt.Errorf("%w: ReadAt requires io.ReaderAt for seekable files", fs.ErrNotSupported))
		return
	} else if reader, ok := f.(interface {
		ReadContext(context.Context, []byte) (int, error)
	}); ok {
		n, err = reader.ReadContext(s.task.Context(), buf)
	} else {
		n, err = f.Read(buf)
	}
	if n < 0 || n > len(buf) {
		r.Return(fmt.Errorf("ReadAt returned invalid count %d for buffer length %d", n, len(buf)))
		return
	}
	// The RPC returns bytes or an error, not both. Deliver partial data even
	// when the backend also reports an error; no deferred error is retained.
	if n == 0 && err != nil && err != io.EOF {
		r.Return(err)
		return
	}
	r.Return(buf[:n])
}
