import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

// Run the source without the bundler's text-loader imports or browser APIs.
const source = (await readFile(new URL("./kernel.js", import.meta.url), "utf8"))
    .replace(/^import .*;$/gm, "")
    .replace("export class WanixKernel", "class WanixKernel")
    .replaceAll("import.meta.url", JSON.stringify(import.meta.url));

for (const runtime of ["preloaded", "Go", "TinyGo"]) {
    test(`load uses ${runtime} runtime`, async () => {
        const scripts = [];
        const instances = [];
        const runs = [];
        class Go {
            constructor() {
                this.importObject = {};
                instances.push(this);
            }
            run(instance) { runs.push(instance); }
        }
        const window = runtime === "preloaded" ? { Go } : {};
        const instance = {};
        const context = vm.createContext({
            URL, Uint8Array, TextDecoder, console, window,
            wasmExecGo: "go-runtime", wasmExecTinygo: "tinygo-runtime",
            document: {
                createElement(tag) {
                    assert.notEqual(runtime, "preloaded", "preloaded runtime must not inject inline script");
                    assert.equal(tag, "script");
                    return {};
                },
                head: { appendChild(script) { scripts.push(script.textContent); window.Go = Go; } },
            },
            WebAssembly: { async instantiate(bytes, imports) {
                assert.equal(imports.wanix.getInstanceID(), 1);
                assert.equal(bytes[0], runtime === "TinyGo" ? 97 : 0);
                return { instance };
            } },
        });
        vm.runInContext(source + "\nglobalThis.Kernel = WanixKernel;", context);
        const kernel = new context.Kernel({ hasAttribute() { return false; } });
        const bytes = runtime === "TinyGo" ? new TextEncoder().encode("asyncify_start_unwind") : new Uint8Array([0]);
        await kernel.load(bytes.buffer);
        assert.equal(instances.length, 1);
        assert.deepEqual(runs, [instance]);
        assert.deepEqual(scripts, runtime === "preloaded" ? [] : [runtime === "Go" ? "go-runtime" : "tinygo-runtime"]);
    });
}
