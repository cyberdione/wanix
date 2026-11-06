package wasmutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectTypeImports(t *testing.T) {
	for _, tt := range []struct{ module, want string }{
		{"ferrule.linux.syscall", "linux"},
		{"wasi_snapshot_preview1", "wasi"},
		{"gojs", "gojs"},
		{"env", "gojs"},
	} {
		t.Run(tt.module, func(t *testing.T) {
			// One function import: module.syscall, type index zero.
			imports := append([]byte{1, byte(len(tt.module))}, []byte(tt.module)...)
			imports = append(imports, 7)
			imports = append(imports, []byte("syscall")...)
			imports = append(imports, 0, 0)
			bin := append([]byte{0, 97, 115, 109, 1, 0, 0, 0, 2, byte(len(imports))}, imports...)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "task.wasm"), bin, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := DetectType(os.DirFS(dir), "task.wasm")
			if err != nil || got != tt.want {
				t.Fatalf("DetectType = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
