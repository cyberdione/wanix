//go:build ignore
// +build ignore

package main

import (
	"fmt"
 "flag"
 "path/filepath"
	"os"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
 "tractor.dev/wanix/internal/cbornoeval"
)

func main() {
    wrapperOnly := flag.Bool("wrapper-only", false, "build only the wrapper")
    wrapperOutput := flag.String("wrapper-output", "", "exclusive wrapper output outside checkout")
    flag.Parse()
    if !*wrapperOnly && *wrapperOutput != "" { panic("wrapper-output requires wrapper-only") }

	opts := api.BuildOptions{
		Bundle:   true,
		Write:    true,
		Format:   api.FormatESModule,
		External: []string{"util"},
		LogLevel: api.LogLevelInfo,
        Plugins: []api.Plugin{cbornoeval.Plugin()},
	}

	wanixOpts := opts
	wanixOpts.EntryPoints = []string{"index.ts"}
	wanixOpts.Outfile = "dist/wanix.js"
	wanixOpts.Loader = map[string]api.Loader{
		".go.js":     api.LoaderText,
		".tinygo.js": api.LoaderText,
		".css":       api.LoaderText,
	}
    if *wrapperOnly {
        root, err := os.Getwd()
        if err != nil { panic(err) }
        out := *wrapperOutput
        if !filepath.IsAbs(out) { panic("wrapper output must be absolute") }
        rel, err := filepath.Rel(root, out)
        if err != nil { panic(err) }
        if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) { panic("wrapper output must be outside checkout") }
        wanixOpts.Write = false
        wanixOpts.Outfile = out
        result := api.Build(wanixOpts)
        if len(result.Errors) != 0 { os.Exit(1) }
        if len(result.OutputFiles) != 1 || result.OutputFiles[0].Path != out { panic("expected exactly one wrapper output") }
        contents := result.OutputFiles[0].Contents
        if len(contents) > 4*1024*1024 { panic("wrapper exceeds 4 MiB cap") }
        f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
        if err != nil { panic(err) }
        if _, err := f.Write(contents); err != nil { f.Close(); panic(err) }
        if err := f.Close(); err != nil { panic(err) }
        return
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

	fmt.Println("js build complete")
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
