import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import vm from "node:vm";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { TextEncoder, TextDecoder } from "node:util";

const here = dirname(fileURLToPath(import.meta.url));
const sourcePath = resolve(here, "../../wasi/worker/worker.js");
let source = await readFile(sourcePath, "utf8");
source = source.replace(/^import\s*\{[\s\S]*?\} from "\.\/lib\.js";\n/, "");
source += "\nglobalThis.__messageHandler = messageHandler; globalThis.__nativePathRename = nativePathRename; globalThis.__runWasi = runWasi;\n";
let wasiFds;
class FakeFile { constructor(handle) { this.handle = handle; } }
class FakeFileHandle { constructor(caller, path) { this.caller = caller; this.path = path; } }
class FakeOpenFile { constructor(file) { this.file = file; } }
class FakeWASI {
  constructor(args, env, fds) { this.args = args; this.env = env; wasiFds = fds; this.inst = { exports: { memory: new WebAssembly.Memory({ initial: 1 }) } }; this.wasiImport = {}; }
  start() { return 0; }
}
class FakeGo { constructor() { this.importObject = {}; } }
const fakeWebAssembly = { Memory: WebAssembly.Memory, async compile() { return {}; }, async instantiate() { return { exports: { memory: new WebAssembly.Memory({ initial: 1 }) } }; } };
const context = vm.createContext({ console, self: {}, globalThis: {}, global: {}, TextEncoder, TextDecoder, performance: { now: () => 0 }, setTimeout,
  WASI: FakeWASI, OpenFile: FakeOpenFile, File: FakeFile, FileHandle: FakeFileHandle, OpenEmptyFile: class {},
  Directory: class { constructor(handle) { this.handle = handle; } }, DirectoryHandle: FakeFileHandle,
  PreopenDirectory: class { constructor(path, dir) { this.path = path; this.dir = dir; } },
  applyPatchPollOneoff() {},
  CallBuffer: class { constructor() { return { call(method, args) { if (method === "exit") context.__exit = args.code; } }; } },
  Go: FakeGo, WebAssembly: fakeWebAssembly, applyPatchPollOneoff() {},
});
context.globalThis = context;
vm.runInContext(source, context, { filename: sourcePath });

const nativePathRename = context.__nativePathRename;
assert.equal(typeof nativePathRename, "function");
const memory = new WebAssembly.Memory({ initial: 1 });
const bytes = new Uint8Array(memory.buffer);
bytes.set(new TextEncoder().encode("/old"), 32);
bytes.set(new TextEncoder().encode("/new"), 40);
const fakeWasi = { inst: { exports: { memory } } };
const renameCalls = [];
assert.equal(nativePathRename(fakeWasi, { call(method, args) { renameCalls.push([method, args]); } }, 3, 32, 4, 3, 40, 4), 0);
assert.equal(JSON.stringify(renameCalls), JSON.stringify([["path_rename", { from: "old", to: "new" }]]));
assert.equal(nativePathRename(fakeWasi, { call() { throw new Error("no"); } }, 2, 32, 3, 3, 40, 3), 76);
assert.equal(nativePathRename(fakeWasi, { call() { throw new Error("no"); } }, 3, 32, 3, 3, 40, 3), 6);

const handlerFactory = context.__messageHandler;
assert.equal(typeof handlerFactory, "function");

const responses = [];
const call = { respond(value) { responses.push(value); } };
const failingFs = { open() { return Promise.reject(new Error("missing")); } };
const handler = handlerFactory(failingFs, call, "t");
await handler({ data: { method: "path_open", path: "missing" } });
assert.equal(JSON.stringify(responses), JSON.stringify([{ __wanix_error: "Error: missing" }]));

responses.length = 0;
const workingFs = { writeAt() { return Promise.resolve(3); } };
const handler2 = handlerFactory(workingFs, call, "t");
await handler2({ data: { method: "fd_write", fd: 4, data: new Uint8Array([1, 2, 3]), at: 0 } });
assert.equal(JSON.stringify(responses), JSON.stringify([3]));


responses.length = 0;
const renamed = [];
const renameFs = { rename(from, to) { renamed.push([from, to]); return Promise.resolve(); } };
const handler3 = handlerFactory(renameFs, call, "t");
await handler3({ data: { method: "path_rename", from: "a", to: "b" } });
assert.deepEqual(renamed, [["a", "b"]]);
assert.deepEqual(responses, [true]);

responses.length = 0;
const failRenameFs = { rename() { return Promise.reject(new Error("destination exists")); } };
const handler4 = handlerFactory(failRenameFs, call, "t");
await handler4({ data: { method: "path_rename", from: "a", to: "b" } });
assert.equal(JSON.stringify(responses), JSON.stringify([{ __wanix_error: "Error: destination exists" }]));

responses.length = 0;
await handler3({ data: { method: "not_supported" } });
assert.equal(JSON.stringify(responses), JSON.stringify([{ __wanix_error: "unsupported method: not_supported" }]));

// Reads forward explicit positions (including zero) and cap both API paths
// before consuming any bytes from the backend.
responses.length = 0;
const readCalls = [];
const readFs = {
  async read(fd, count) { readCalls.push(["read", fd, count]); return new Uint8Array([1]); },
  async readAt(fd, count, at) { readCalls.push(["readAt", fd, count, at]); return new Uint8Array([2]); },
};
const readHandler = handlerFactory(readFs, { maxData: 8, respond(value) { responses.push(value); } }, "t");
await readHandler({ data: { method: "fd_read", fd: 4, count: 32, at: 0 } });
await readHandler({ data: { method: "fd_read", fd: 4, count: 3, at: 17 } });
await readHandler({ data: { method: "fd_read", fd: 0, count: 32 } });
assert.deepEqual(readCalls, [["readAt", 4, 8, 0], ["readAt", 4, 3, 17], ["read", 0, 8]]);
assert.deepEqual(responses.map(value => Array.from(value)), [[2], [2], [1]]);
responses.length = 0;
const cancelledReadHandler = handlerFactory({ async readAt() { throw new Error("context canceled"); } },
  { maxData: 8, respond(value) { responses.push(value); } }, "t");
await cancelledReadHandler({ data: { method: "fd_read", fd: 0, count: 1, at: 0 } });
assert.equal(JSON.stringify(responses), JSON.stringify([{ __wanix_error: "Error: context canceled" }]));

const runWasi = context.__runWasi;
assert.equal(typeof runWasi, "function");
await runWasi({ data: { args: ["app.wasm"], env: [], buffer: new SharedArrayBuffer(16), bin: new ArrayBuffer(8), stdin: "#task/7/fd/0", stdout: "#task/7/fd/1", stderr: "#task/7/fd/2" } });
assert.equal(wasiFds.length, 4);
assert.equal(wasiFds[0].file.handle.path, "#task/7/fd/0");
assert.equal(wasiFds[1].file.handle.path, "#task/7/fd/1");
assert.equal(wasiFds[2].file.handle.path, "#task/7/fd/2");
