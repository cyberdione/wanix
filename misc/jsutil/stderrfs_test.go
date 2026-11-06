//go:build js && wasm

package jsutil

import (
	"errors"
	"reflect"
	"syscall/js"
	"testing"

	"tractor.dev/wanix/fs"
)

func TestStderrSeparateFromConsole(t *testing.T) {
	console := js.Global().Get("console")
	oldLog, oldError := console.Get("log"), console.Get("error")
	var stdout, stderr []string
	logFn := js.FuncOf(func(_ js.Value, args []js.Value) any { stdout = append(stdout, args[0].String()); return nil })
	errorFn := js.FuncOf(func(_ js.Value, args []js.Value) any { stderr = append(stderr, args[0].String()); return nil })
	console.Set("log", logFn)
	console.Set("error", errorFn)
	defer func() { console.Set("log", oldLog); console.Set("error", oldError); logFn.Release(); errorFn.Release() }()

	sink := &stderrFS{w: new(stderrWriter)}
	if _, err := sink.Open("missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open missing: %v", err)
	}
	first, err := sink.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sink.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	h := first.(*stderrHandle)
	if n, err := h.WriteAt([]byte("err"), 99); n != 3 || err != nil {
		t.Fatalf("WriteAt: %d, %v", n, err)
	}
	if _, err := h.Read(nil); err != fs.ErrPermission {
		t.Fatalf("Read: %v", err)
	}
	h.Write([]byte("or\r\npartial"))
	first.Close()
	if !reflect.DeepEqual(stderr, []string{"error"}) {
		t.Fatalf("before last close: %q", stderr)
	}
	second.Close()
	out := &consoleFS{w: new(consoleWriter)}
	oh, err := out.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	oh.(*consoleHandle).Write([]byte("output\n"))
	oh.Close()
	if !reflect.DeepEqual(stderr, []string{"error", "partial"}) {
		t.Fatalf("stderr: %q", stderr)
	}
	if !reflect.DeepEqual(stdout, []string{"output"}) {
		t.Fatalf("stdout: %q", stdout)
	}
}
