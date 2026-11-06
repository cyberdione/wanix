package scratchfs

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
)

func TestExistingSizeBoundaries(t *testing.T) {
	for _, ceiling := range []uint64{2147483647, 9007199254740991} {
		for _, delta := range []int64{-1, 0, 1} {
			err := checkExistingSize(int64(ceiling)+delta, ceiling)
			if (delta > 0) != errors.Is(err, syscall.EOVERFLOW) {
				t.Fatalf("%d %+d: %v", ceiling, delta, err)
			}
		}
	}
}
func TestBoundedOpenBeforeMutation(t *testing.T) {
	for _, trunc := range []bool{false, true} {
		for _, create := range []bool{false, true} {
			f := New()
			if e := f.WriteFile("leaf", []byte("keep"), 0611); e != nil {
				t.Fatal(e)
			}
			before, _ := f.Stat("leaf")
			flags := os.O_RDWR
			if trunc {
				flags |= os.O_TRUNC
			}
			if create {
				flags |= os.O_CREATE
			}
			h, e := f.OpenFileGrantedBounded("leaf", flags, 0777, true, true, 3)
			if h != nil || !errors.Is(e, syscall.EOVERFLOW) {
				t.Fatalf("%v %v", h, e)
			}
			after, _ := f.Stat("leaf")
			if before.Size() != after.Size() || before.Sys() != after.Sys() || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("metadata mutated")
			}
			old := mustOpen(t, f, "leaf", os.O_RDONLY)
			if bytesAt(t, old) != "keep" {
				t.Fatal("bytes mutated")
			}
			h, e = f.OpenFileGrantedBounded("leaf", flags|os.O_CREATE|os.O_EXCL, 0, true, true, 0)
			if h != nil || !errors.Is(e, os.ErrExist) {
				t.Fatal("exclusive precedence", e)
			}
			h, e = f.OpenFileGrantedBounded("new", os.O_CREATE|os.O_RDWR, 0600, true, true, 0)
			if e != nil {
				t.Fatal(e)
			}
			h.Close()
			h, e = f.OpenFileGrantedBounded("leaf", flags, 0, true, true, 4)
			if e != nil {
				t.Fatal(e)
			}
			h.Close()
		}
	}
}

// Resizing and bounded truncation must observe one complete inode state.
func TestBoundedOpenConcurrentResize(t *testing.T) {
	f := New()
	if e := f.WriteFile("leaf", []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if e := f.Truncate("leaf", 5); e != nil {
				t.Error(e)
			}
			if e := f.Truncate("leaf", 1); e != nil {
				t.Error(e)
			}
		}
	}()
	for i := 0; i < 200; i++ {
		h, e := f.OpenFileGrantedBounded("leaf", os.O_RDWR|os.O_TRUNC, 0, true, true, 1)
		if e != nil && !errors.Is(e, syscall.EOVERFLOW) {
			t.Fatal(e)
		}
		if h != nil {
			h.Close()
		}
	}
	wg.Wait()
}
