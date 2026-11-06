// Diagnostic transformed-source closure unit; NOT module import or wire coverage.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import vm from "node:vm";
import { exactStatSize } from "./statx-core.js";
const hash = bytes => createHash("sha256").update(bytes).digest("hex");
const source = await readFile(new URL("./worker.js", import.meta.url), "utf8");
assert.equal(hash(source), "f1a24d8c446221b5ca39610be6892d82c5fbb2a8bb90a74f8589c623b936ebc6");
const startMarker = "function messageHandler(";
const endMarker = "\nfunction extractWasm(";
assert.equal(source.split(startMarker).length, 2);
assert.equal(source.split(endMarker).length, 2);
const start = source.indexOf(startMarker);
const separator = source.indexOf(endMarker, start);
// Include the one LF separator in the RAW pin; never trim source.
const closure = source.slice(start, separator + 1);
assert.equal(hash(closure), "a8040306bdee28af5254e4c135158ce9f93962c0d30b8104379319fd194f6849");
const core = await readFile(new URL("./statx-core.js", import.meta.url));
assert.equal(hash(core), "3c326cd967e23f92100962b42976b26c4c03bcdf34817f06487749f6c169bd6c");
// Host Error injection makes actual instanceof Error branch agree with host fakes.
const context = vm.createContext({ exactStatSize, Error });
vm.runInContext(closure + "\nglobalThis.handlerFactory = messageHandler;", context);
const cases = [
 { name: "zero", stat: { Size: 0 }, value: 0 },
 { name: "bigint zero", stat: { Size: 0n }, value: 0 },
 { name: "number", stat: { Size: 37 }, value: 37 },
 { name: "null stat", stat: null, error: "missing stat metadata" },
 { name: "missing size", stat: {}, error: "invalid stat size" },
 { name: "unsafe", stat: { Size: 9007199254740992n }, error: "stat size overflow" },
 { name: "backend Error", failure: new Error("backend missing"), error: "backend missing" },
 { name: "backend string", failure: "backend denied", error: "backend denied" },
];
for (const c of cases) {
 const calls = [], responses = [];
 const fs = { async stat(path) { calls.push(path); if ("failure" in c) throw c.failure; return c.stat; } };
 const handler = context.handlerFactory(fs, { respond(value) { responses.push(value); } }, "diagnostic");
 await handler({ data: { method: "path_size", path: "file" } });
 assert.deepEqual(calls, ["file"], c.name);
 assert.equal(responses.length, 1, c.name);
 if ("error" in c) {
  assert.equal(responses[0].__wanix_error, c.error, c.name);
  assert.equal(Object.keys(responses[0]).join(","), "__wanix_error", c.name);
 } else assert.equal(responses[0], c.value, c.name);
}
console.log("8 actual producer closure cases passed (transformed-source unit only)");
