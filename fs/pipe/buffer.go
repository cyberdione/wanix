package pipe

import (
	"bytes"
	"context"
	"io"
	"sync"
)

type Buffer struct {
	buffer   bytes.Buffer
	mu       sync.Mutex
	dataCond *sync.Cond
	closed   bool
	block    bool
}

func NewBuffer(block bool) *Buffer {
	bp := &Buffer{block: block}
	bp.dataCond = sync.NewCond(&bp.mu)
	return bp
}

func (bp *Buffer) Write(data []byte) (int, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	if bp.closed {
		return 0, io.ErrClosedPipe
	}

	n, err := bp.buffer.Write(data)
	bp.dataCond.Signal()
	return n, err
}

func (bp *Buffer) Read(p []byte) (int, error) { return bp.ReadContext(context.Background(), p) }

func (bp *Buffer) ReadContext(ctx context.Context, p []byte) (int, error) {
	stop := context.AfterFunc(ctx, func() { bp.mu.Lock(); bp.dataCond.Broadcast(); bp.mu.Unlock() })
	defer stop()
	bp.mu.Lock()
	defer bp.mu.Unlock()

	if bp.block {
		for bp.buffer.Len() == 0 && !bp.closed && ctx.Err() == nil {
			bp.dataCond.Wait()
		}
	}

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if bp.closed && bp.buffer.Len() == 0 {
		return 0, io.EOF
	}

	return bp.buffer.Read(p)
}

func (bp *Buffer) Close() error {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	bp.closed = true
	bp.dataCond.Broadcast()
	return nil
}

func (bp *Buffer) Size() int {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.buffer.Len()
}
