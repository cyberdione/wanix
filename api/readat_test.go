package api

import (
	"bytes"
	"context"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
)

func TestReadAtPositionAndEOF(t *testing.T) {
	client, task := startTestServerWithTask(t)
	fd := task.OpenFD(&positionedFile{Reader: bytes.NewReader([]byte("abcdef"))}, "file")
	for _, tc := range []struct {
		offset uint64
		want   string
	}{{2, "cdef"}, {6, ""}, {0, "abcdef"}} {
		var got []byte
		if _, err := client.Call(context.Background(), "ReadAt", []any{uint64(fd), uint64(8), tc.offset}, &got); err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Fatalf("offset %d = %q, want %q", tc.offset, got, tc.want)
		}
	}
	var got []byte
	if _, err := client.Call(context.Background(), "Read", []any{uint64(fd), uint64(2)}, &got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "ab" {
		t.Fatalf("positioned read moved cursor: %q", got)
	}
}

type positionedFile struct{ *bytes.Reader }

func (*positionedFile) Close() error               { return nil }
func (*positionedFile) Stat() (fs.FileInfo, error) { return fskit.Entry("file", 0600), nil }

type seekFile struct{ io.ReadSeeker }

func (*seekFile) Close() error               { return nil }
func (*seekFile) Stat() (fs.FileInfo, error) { return fskit.Entry("file", 0600), nil }

func TestReadAtSeekOnlyUnsupported(t *testing.T) {
	client, task := startTestServerWithTask(t)
	file := &seekFile{bytes.NewReader([]byte("abcdef"))}
	if _, err := file.Seek(1, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	fd := task.OpenFD(file, "file")
	var got []byte
	if _, err := client.Call(context.Background(), "ReadAt", []any{uint64(fd), uint64(2), uint64(2)}, &got); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected unsupported error, got %v", err)
	}
	if _, err := client.Call(context.Background(), "Read", []any{uint64(fd), uint64(2)}, &got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "bc" {
		t.Fatalf("unsupported positioned read moved cursor: %q", got)
	}
}

func TestReadAtStreamFallback(t *testing.T) {
	client, task := startTestServerWithTask(t)
	file := fskit.NewStreamFile(eofReader{bytes.NewReader([]byte("abcdef"))}, nil, nil, fskit.Entry("stream", 0600))
	fd := task.OpenFD(file, "stream")
	var got []byte
	if _, err := client.Call(context.Background(), "ReadAt", []any{uint64(fd), uint64(8), uint64(2)}, &got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcdef" {
		t.Fatalf("got %q want abcdef", got)
	}
}

type offsetProbe struct {
	calls  int
	offset int64
}

func (*offsetProbe) Close() error               { return nil }
func (*offsetProbe) Stat() (fs.FileInfo, error) { return fskit.Entry("file", 0600), nil }
func (f *offsetProbe) Read([]byte) (int, error) { f.calls++; return 0, io.EOF }
func (f *offsetProbe) ReadAt(_ []byte, off int64) (int, error) {
	f.calls++
	f.offset = off
	return 0, io.EOF
}

func TestReadAtOffsetRange(t *testing.T) {
	client, task := startTestServerWithTask(t)
	probe := &offsetProbe{}
	fd := task.OpenFD(probe, "file")
	var got []byte
	for _, offset := range []uint64{uint64(math.MaxInt64) + 1, math.MaxUint64} {
		if _, err := client.Call(context.Background(), "ReadAt", []any{uint64(fd), uint64(1), offset}, &got); err == nil || !strings.Contains(err.Error(), "offset exceeds MaxInt64") {
			t.Fatalf("offset %d: expected range error, got %v", offset, err)
		}
	}
	// Hide ReaderAt to exercise the sequential stream path too.
	streamFD := task.OpenFD(struct{ fs.File }{probe}, "stream")
	if _, err := client.Call(context.Background(), "ReadAt", []any{uint64(streamFD), uint64(1), uint64(math.MaxUint64)}, &got); err == nil {
		t.Fatal("stream accepted out-of-range offset")
	}
	if probe.calls != 0 {
		t.Fatalf("invalid offset reached backend %d times", probe.calls)
	}
	if _, err := client.Call(context.Background(), "ReadAt", []any{uint64(fd), uint64(1), uint64(math.MaxInt64)}, &got); err != nil {
		t.Fatal(err)
	}
	if probe.calls != 1 || probe.offset != math.MaxInt64 {
		t.Fatalf("boundary offset not forwarded: %+v", probe)
	}
}

type cancelledReadFile struct{ started chan struct{} }

func (*cancelledReadFile) Close() error               { return nil }
func (*cancelledReadFile) Stat() (fs.FileInfo, error) { return fskit.Entry("stream", 0600), nil }
func (*cancelledReadFile) Read([]byte) (int, error)   { panic("ReadContext bypassed") }
func (f *cancelledReadFile) ReadContext(ctx context.Context, p []byte) (int, error) {
	close(f.started)
	<-ctx.Done()
	return 0, ctx.Err()
}
func TestReadAtStreamTaskCancellation(t *testing.T) {
	client, task := startTestServerWithTask(t)
	f := &cancelledReadFile{make(chan struct{})}
	fd := task.OpenFD(f, "stream")
	done := make(chan error, 1)
	go func() {
		var got []byte
		_, err := client.Call(context.Background(), "ReadAt", []any{uint64(fd), uint64(8), uint64(0)}, &got)
		done <- err
	}()
	select {
	case <-f.started:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	task.Exit("130")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
	case <-time.After(time.Second):
		t.Fatal("read did not cancel")
	}
}
