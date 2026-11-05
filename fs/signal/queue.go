package signal

import (
	"context"
	"errors"
	"io"
	"sync"
)

var ErrSaturated = errors.New("signal: queue saturated")

// Queue is a single-consumer, bounded, byte-stream channel. Writes are atomic
// frames; reads may split frames. A full queue rejects the entire write.
type Queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	limit  int
	closed bool
}

func NewQueue(limit int) *Queue {
	if limit <= 0 {
		panic("signal: queue limit must be positive")
	}
	q := &Queue{limit: limit}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *Queue) Write(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(p) > q.limit-len(q.buf) {
		return 0, ErrSaturated
	}
	q.buf = append(q.buf, p...)
	q.cond.Broadcast()
	return len(p), nil
}

func (q *Queue) Read(p []byte) (int, error) { return q.ReadContext(context.Background(), p) }

func (q *Queue) ReadContext(ctx context.Context, p []byte) (int, error) {
	stop := context.AfterFunc(ctx, func() { q.mu.Lock(); q.cond.Broadcast(); q.mu.Unlock() })
	defer stop()
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for len(q.buf) == 0 && !q.closed && ctx.Err() == nil {
		q.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(q.buf) == 0 && q.closed {
		return 0, io.EOF
	}
	n := copy(p, q.buf)
	q.buf = q.buf[n:]
	q.cond.Broadcast()
	return n, nil
}

// Close preserves queued bytes; readers drain them before EOF.
func (q *Queue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	q.closed = true
	q.cond.Broadcast()
	return nil
}
