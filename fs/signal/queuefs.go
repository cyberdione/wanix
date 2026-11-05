package signal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"strings"
	"sync"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
)

// QueueFS exposes one bounded queue as a directory with read, write and ctl
// files. Reads block, writes are atomic bounded frames, and ctl accepts close.
type QueueFS struct {
	Queue      *Queue
	OnClose    func()
	mu         sync.Mutex
	readerOpen bool
	closed     bool
	closeOnce  sync.Once
}

func NewQueueFS(maxBytes int) *QueueFS { return &QueueFS{Queue: NewQueue(maxBytes)} }
func (q *QueueFS) Close() error {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.finishClose()
	return nil
}

func (q *QueueFS) finishClose() {
	q.closeOnce.Do(func() {
		_ = q.Queue.Close()
		if q.OnClose != nil {
			q.OnClose()
		}
	})
}

func (q *QueueFS) StatContext(_ context.Context, name string) (fs.FileInfo, error) {
	switch name {
	case ".":
		return fskit.Entry(".", fs.ModeDir|0555), nil
	case "read":
		q.mu.Lock()
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return nil, fs.ErrNotExist
		}
		return fskit.Entry("read", 0444), nil
	case "write":
		q.mu.Lock()
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return nil, fs.ErrNotExist
		}
		return fskit.Entry("write", 0222), nil
	case "ctl":
		q.mu.Lock()
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return nil, fs.ErrNotExist
		}
		return fskit.Entry("ctl", 0222), nil
	default:
		return nil, fs.ErrNotExist
	}
}

func (q *QueueFS) Open(name string) (fs.File, error) {
	return q.OpenContext(context.Background(), name)
}
func (q *QueueFS) OpenContext(ctx context.Context, name string) (fs.File, error) {
	switch name {
	case ".":
		return fskit.DirFile(fskit.Entry(".", fs.ModeDir|0555), fskit.Entry("read", 0444), fskit.Entry("write", 0222), fskit.Entry("ctl", 0222)), nil
	case "read":
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.closed {
			return nil, fs.ErrNotExist
		}
		if q.readerOpen {
			return nil, fs.ErrExist
		}
		q.readerOpen = true
		r := &queueReader{owner: q, ctx: ctx}
		if ctx.Done() != nil {
			context.AfterFunc(ctx, func() { _ = q.Close() })
		}
		return r, nil
	case "write":
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.closed {
			return nil, fs.ErrNotExist
		}
		return &queueWriter{q: q}, nil
	case "ctl":
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.closed {
			return nil, fs.ErrNotExist
		}
		return &queueControl{q: q}, nil
	default:
		return nil, fs.ErrNotExist
	}
}

type queueReader struct {
	owner  *QueueFS
	ctx    context.Context
	mu     sync.Mutex
	closed bool
}

func (r *queueReader) Read(p []byte) (int, error) { return r.ReadContext(r.ctx, p) }
func (r *queueReader) ReadContext(ctx context.Context, p []byte) (int, error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return 0, fs.ErrClosed
	}
	n, err := r.owner.Queue.ReadContext(ctx, p)
	if ctx.Err() != nil {
		_ = r.owner.Close()
	}
	return n, err
}
func (r *queueReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	r.owner.mu.Lock()
	r.owner.closed = true
	r.owner.readerOpen = false
	r.owner.mu.Unlock()
	r.owner.finishClose()
	return nil
}
func (r *queueReader) Stat() (fs.FileInfo, error) { return fskit.Entry("read", 0444), nil }

type queueWriter struct {
	q      *QueueFS
	closed bool
	mu     sync.Mutex
}

func (w *queueWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, fs.ErrClosed
	}
	return w.q.Queue.Write(p)
}
func (w *queueWriter) Read([]byte) (int, error)   { return 0, fs.ErrPermission }
func (w *queueWriter) Close() error               { w.mu.Lock(); w.closed = true; w.mu.Unlock(); return nil }
func (w *queueWriter) Stat() (fs.FileInfo, error) { return fskit.Entry("write", 0222), nil }

type queueControl struct {
	q      *QueueFS
	closed bool
	mu     sync.Mutex
}

func (c *queueControl) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, fs.ErrClosed
	}
	if string(p) != "close" && string(p) != "close\n" {
		return 0, fs.ErrInvalid
	}
	if err := c.q.Close(); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (c *queueControl) Read([]byte) (int, error)   { return 0, fs.ErrPermission }
func (c *queueControl) Close() error               { c.mu.Lock(); c.closed = true; c.mu.Unlock(); return nil }
func (c *queueControl) Stat() (fs.FileInfo, error) { return fskit.Entry("ctl", 0222), nil }

var _ fs.FS = (*QueueFS)(nil)
var _ fs.OpenContextFS = (*QueueFS)(nil)
var _ io.Reader = (*queueReader)(nil)

// QueueAllocator allocates independent bounded queues under numeric IDs.
type QueueAllocator struct {
	mu        sync.Mutex
	queues    map[string]*QueueFS
	MaxBytes  int
	MaxQueues int
}

func (a *QueueAllocator) Open(name string) (fs.File, error) {
	return a.OpenContext(context.Background(), name)
}

func (a *QueueAllocator) Route(_ context.Context, name string) (fs.FS, string, error) {
	id, rest, ok := strings.Cut(name, "/")
	if !ok {
		id, rest = name, "."
	}
	a.mu.Lock()
	q := a.queues[id]
	a.mu.Unlock()
	if q == nil {
		return a, name, nil
	}
	return q, rest, nil
}

// BindAllocFS exposes the allocator root or an allocated queue directory.
func (a *QueueAllocator) BindAllocFS(name string) (fs.FS, error) {
	if name == "." {
		return a, nil
	}
	if _, rest, ok := strings.Cut(name, "/"); ok {
		name = rest
	}
	a.mu.Lock()
	q := a.queues[name]
	a.mu.Unlock()
	if q == nil {
		return nil, fs.ErrNotExist
	}
	return q, nil
}
func (a *QueueAllocator) OpenContext(ctx context.Context, name string) (fs.File, error) {
	if name == "." {
		return fskit.DirFile(fskit.Entry(".", fs.ModeDir|0555), fskit.Entry("new", 0444)), nil
	}
	if name == "new" {
		if a.MaxBytes <= 0 {
			return nil, fs.ErrInvalid
		}
		return &fskit.FuncFile{Node: fskit.Entry("new", 0444), ReadFunc: func(n *fskit.Node) error {
			idBytes := make([]byte, 16)
			if _, err := rand.Read(idBytes); err != nil {
				return err
			}
			id := hex.EncodeToString(idBytes)
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.queues == nil {
				a.queues = make(map[string]*QueueFS)
			}
			maxQueues := a.MaxQueues
			if maxQueues <= 0 {
				maxQueues = 64
			}
			if len(a.queues) >= maxQueues {
				return ErrSaturated
			}
			if _, exists := a.queues[id]; exists {
				return fs.ErrExist
			}
			q := NewQueueFS(a.MaxBytes)
			q.OnClose = func() { a.mu.Lock(); delete(a.queues, id); a.mu.Unlock() }
			a.queues[id] = q
			fskit.SetData(n, []byte(id+"\n"))
			return nil
		}}, nil
	}
	id, rest, ok := strings.Cut(name, "/")
	if !ok {
		return nil, fs.ErrNotExist
	}
	a.mu.Lock()
	q := a.queues[id]
	a.mu.Unlock()
	if q == nil {
		return nil, fs.ErrNotExist
	}
	return q.OpenContext(ctx, rest)
}
