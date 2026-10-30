package fs

import (
	"bytes"
	"io"
	iofs "io/fs"
	"testing"
)

type writeTestFile struct {
	data bytes.Buffer
}

func (f *writeTestFile) Read([]byte) (int, error)     { return 0, io.EOF }
func (f *writeTestFile) Close() error                 { return nil }
func (f *writeTestFile) Stat() (iofs.FileInfo, error) { return nil, nil }
func (f *writeTestFile) Write(p []byte) (int, error)  { return f.data.Write(p) }

type seekWriteTestFile struct {
	writeTestFile
	seeks int
}

func (f *seekWriteTestFile) Seek(offset int64, whence int) (int64, error) {
	f.seeks++
	return offset, nil
}

func TestWriteAtSequentialWriter(t *testing.T) {
	f := new(writeTestFile)

	if n, err := WriteAt(f, []byte("first"), 0); err != nil || n != 5 {
		t.Fatalf("WriteAt(offset 0) = (%d, %v), want (5, nil)", n, err)
	}
	if n, err := WriteAt(f, []byte("second"), 5); err != nil || n != 6 {
		t.Fatalf("WriteAt(offset 5) = (%d, %v), want (6, nil)", n, err)
	}
	if got := f.data.String(); got != "firstsecond" {
		t.Fatalf("data = %q, want %q", got, "firstsecond")
	}
}

func TestWriteAtSeeksSeekableWriter(t *testing.T) {
	f := new(seekWriteTestFile)

	if n, err := WriteAt(f, []byte("data"), 4); err != nil || n != 4 {
		t.Fatalf("WriteAt = (%d, %v), want (4, nil)", n, err)
	}
	if f.seeks != 1 {
		t.Fatalf("Seek calls = %d, want 1", f.seeks)
	}
}
