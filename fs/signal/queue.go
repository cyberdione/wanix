package signal

import (
	"context"
	"errors"
	"io"
	"sync"
)

var ErrSaturated = errors.New("signal: queue saturated")

// Queue is a single-consumer, bounded, byte-stream channel. Write is an
// atomic frame: it accepts all of p or rejects the whole write with
// ErrSaturated once the remaining window does not fit (host-side producers
// surface busy). WriteContext carries plan9 pipe write semantics instead:
// it blocks until every byte is admitted and never drops bytes silently
// (task-facing producers, e.g. the wsys cons output queue).
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

// WriteContext blocks until all of p is admitted to the queue, accepting
// capacity-sized frames in order, and returns len(p), nil. It never drops
// bytes: a full queue parks the writer until a reader drains (backpressure,
// plan9 pipe write). The queue's lifetime bounds the park: a Close fails
// the parked write with io.ErrClosedPipe, reporting the bytes already
// admitted as the count — no retry is possible against a closed queue, so
// the torn count cannot duplicate bytes at a retrying caller. A cancelled
// ctx fails the write the same way; callers that can retry after a
// cancellation must not re-send a partially admitted write (the count is
// authoritative) or must pass a context they only cancel on teardown.
func (q *Queue) WriteContext(ctx context.Context, p []byte) (int, error) {
	stop := context.AfterFunc(ctx, func() { q.mu.Lock(); q.cond.Broadcast(); q.mu.Unlock() })
	defer stop()
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}
	total := 0
	for total < len(p) {
		for len(q.buf) == q.limit && !q.closed && ctx.Err() == nil {
			q.cond.Wait()
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if q.closed {
			return total, io.ErrClosedPipe
		}
		n := min(len(p)-total, q.limit-len(q.buf))
		q.buf = append(q.buf, p[total:total+n]...)
		total += n
		q.cond.Broadcast()
	}
	return total, nil
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
