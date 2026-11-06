package cbornoeval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectUnexpectedInputs(t *testing.T) {
	for _, path := range []string{"/tmp/decode.js", "/tmp/node_modules/@progrium/duplex/esm/vnd/cbor-x-1.4.1/decode.js", "/tmp/node_modules/@progrium/duplex/script/vnd/cbor-x-1.4.1/decode.js"} {
		if _, err := Transform(path, []byte("modified")); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
}

func TestPinnedOutputsAndAlteredPatchRejected(t *testing.T) {
	root := os.Getenv("CBOR_INPUT_ROOT")
	if root == "" {
		t.Fatal("CBOR_INPUT_ROOT required")
	}
	original := append([]byte(nil), patchData...)
	defer func() { patchData = original }()
	var patches []patch
	if err := json.Unmarshal(original, &patches); err != nil {
		t.Fatal(err)
	}
	for _, p := range patches {
		path := filepath.Join(root, p.Path)
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		patchData = original
		if _, err := Transform(path, source); err != nil {
			t.Fatal(err)
		}
		changed := append([]patch(nil), patches...)
		for i := range changed {
			if changed[i].Path == p.Path {
				changed[i].New = "// altered output\n"
			}
		}
		patchData, err = json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Transform(path, source); err == nil || !strings.Contains(err.Error(), "output hash mismatch") {
			t.Fatalf("expected output rejection, got %v", err)
		}
	}
}
