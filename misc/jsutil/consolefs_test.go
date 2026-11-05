//go:build js && wasm

package jsutil

import "testing"

func TestConsoleHandleWriteAtAppends(t *testing.T) {
	c := &consoleFS{w: new(consoleWriter)}
	h := &consoleHandle{c: c}

	if n, err := h.WriteAt([]byte("first"), 0); err != nil || n != 5 {
		t.Fatalf("WriteAt(offset 0) = (%d, %v), want (5, nil)", n, err)
	}
	if n, err := h.WriteAt([]byte("second"), 5); err != nil || n != 6 {
		t.Fatalf("WriteAt(offset 5) = (%d, %v), want (6, nil)", n, err)
	}
	if got := string(c.w.buf); got != "firstsecond" {
		t.Fatalf("buffer = %q, want %q", got, "firstsecond")
	}
}
