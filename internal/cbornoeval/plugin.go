package cbornoeval

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

//go:embed patches.json
var patchData []byte

type patch struct{ Path, SHA256, Old, New string }

// Transform accepts only the exact pinned upstream decoder, never a modified install.
func Transform(path string, source []byte) ([]byte, error) {
	var patches []patch
	if err := json.Unmarshal(patchData, &patches); err != nil {
		return nil, err
	}
	for _, p := range patches {
		if !strings.HasSuffix(filepath.ToSlash(path), "/node_modules/"+p.Path) {
			continue
		}
		sum := sha256.Sum256(source)
		if hex.EncodeToString(sum[:]) != p.SHA256 {
			return nil, fmt.Errorf("CBOR no-eval input hash mismatch: %s", path)
		}
		if strings.Count(string(source), p.Old) != 1 {
			return nil, fmt.Errorf("CBOR no-eval patch mismatch: %s", path)
		}
		output := []byte(strings.Replace(string(source), p.Old, p.New, 1))
		expected := ""
		switch p.Path {
		case "@progrium/duplex/esm/vnd/cbor-x-1.4.1/decode.js":
			expected = "8ad1616f35c33bda2316e2cda905400101ff84d9aeb635981e494dfc9ad4abc9"
		case "@progrium/duplex/script/vnd/cbor-x-1.4.1/decode.js":
			expected = "a4260de8b60669a9fa7bf13e982c610b225934f4a97403b887b5021214f3d046"
		}
		outputSum := sha256.Sum256(output)
		if expected == "" || hex.EncodeToString(outputSum[:]) != expected {
			return nil, fmt.Errorf("CBOR no-eval output hash mismatch: %s", path)
		}
		return output, nil
	}
	return nil, fmt.Errorf("unrecognized CBOR decoder path: %s", path)
}

func Plugin() api.Plugin {
	return api.Plugin{Name: "pinned-duplex-cbor-no-eval", Setup: func(build api.PluginBuild) {
		build.OnLoad(api.OnLoadOptions{Filter: `/node_modules/@progrium/duplex/(esm|script)/vnd/cbor-x-1\.4\.1/decode\.js$`}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
			source, err := os.ReadFile(args.Path)
			if err != nil {
				return api.OnLoadResult{}, err
			}
			transformed, err := Transform(args.Path, source)
			if err != nil {
				return api.OnLoadResult{}, err
			}
			text := string(transformed)
			return api.OnLoadResult{Contents: &text, Loader: api.LoaderJS, ResolveDir: filepath.Dir(args.Path)}, nil
		})
	}}
}
