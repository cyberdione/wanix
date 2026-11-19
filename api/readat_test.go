package api

import (
	"bytes"
	"context"
	"io"
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

func TestReadAtSeekAndStreamFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		file fs.File
		want string
	}{
		{"seek", &seekFile{bytes.NewReader([]byte("abcdef"))}, "cdef"},
		{"stream", fskit.NewStreamFile(eofReader{bytes.NewReader([]byte("abcdef"))}, nil, nil, fskit.Entry("stream", 0600)), "abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, task := startTestServerWithTask(t)
			fd := task.OpenFD(tc.file, "file")
			var got []byte
			if _, err := client.Call(context.Background(), "ReadAt", []any{uint64(fd), uint64(8), uint64(2)}, &got); err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
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
