//go:build ignore
// +build ignore

package main

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
)

func main() {
	opts := api.BuildOptions{
		Bundle:   true,
		Write:    true,
		Format:   api.FormatESModule,
		External: []string{"util"},
		LogLevel: api.LogLevelInfo,
	}

	var wg sync.WaitGroup

	handleOpts := opts
	handleOpts.EntryPoints = []string{"api/handle.js"}
	handleOpts.Outfile = "dist/wanix.handle.js"
	wg.Add(1)
	go func() {
		defer wg.Done()
		build(handleOpts)
	}()

	wanixOpts := opts
	wanixOpts.EntryPoints = []string{"index.ts"}
	wanixOpts.Outfile = "dist/wanix.js"
	wanixOpts.Loader = map[string]api.Loader{
		".go.js":     api.LoaderText,
		".tinygo.js": api.LoaderText,
		".css":       api.LoaderText,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		build(wanixOpts)
	}()

	wanixMinOpts := wanixOpts
	wanixMinOpts.Outfile = "dist/wanix.min.js"
	wanixMinOpts.MinifyWhitespace = true
	wanixMinOpts.MinifyIdentifiers = true
	wanixMinOpts.MinifySyntax = true
	wg.Add(1)
	go func() {
		defer wg.Done()
		build(wanixMinOpts)
	}()

	wasiOpts := opts
	wasiOpts.EntryPoints = []string{"wasi/mod.ts"}
	wasiOpts.Outfile = "wasi/worker/lib.js"
	wasiOpts.External = []string{"util"}
	wg.Add(1)
	go func() {
		defer wg.Done()
		build(wasiOpts)
	}()

	// The linux worker shares the wasi worker library surface
	// (WanixHandle/CallBuffer/FileHandle); copy the bundle in place.
	wg.Add(1)
	go func() {
		defer wg.Done()
		wasiOpts := wasiOpts
		wasiOpts.Outfile = "linux/worker/lib.js"
		build(wasiOpts)
	}()

	gojsOpts := opts
	gojsOpts.EntryPoints = []string{"gojs/mod.ts"}
	gojsOpts.Outfile = "gojs/worker/lib.js"
	gojsOpts.External = []string{"util"}
	wg.Add(1)
	go func() {
		defer wg.Done()
		build(gojsOpts)
	}()

	wg.Wait()

	for _, path := range []string{"gojs/worker/lib.js", "wasi/worker/lib.js", "linux/worker/lib.js"} {
		if err := patchCSPDecoder(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Println("js build complete")
}

func patchCSPDecoder(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	source := string(data)
	start := strings.Index(source, "if (this.slowReads++ >= ")
	if start < 0 {
		if strings.Contains(source, "new Function(\"r\"") {
			return fmt.Errorf("unrecognized dynamic CBOR compiler in %s", path)
		}
		return nil
	}
	end := strings.Index(source[start:], "    let object = {};")
	if end < 0 {
		return fmt.Errorf("CBOR object fallback not found in %s", path)
	}
	end += start
	patched := source[:start] + source[end:]
	if strings.Contains(patched, "new Function(\"r\"") {
		return fmt.Errorf("dynamic CBOR compilation remains in %s", path)
	}
	return os.WriteFile(path, []byte(patched), 0644)
}

func build(opts api.BuildOptions) {
	ctx, ctxErr := api.Context(opts)
	if ctxErr != nil {
		fmt.Fprintf(os.Stderr, "%v\n", ctxErr)
		os.Exit(1)
	}
	result := ctx.Rebuild()
	ctx.Dispose()
	if len(result.Errors) > 0 {
		for _, e := range result.Errors {
			fmt.Fprintln(os.Stderr, e.Text)
		}
		os.Exit(1)
	}
}
