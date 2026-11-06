// Diagnostic transformed-source closure unit; NOT module import or wire coverage.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import vm from "node:vm";
import { exactStatSize } from "./statx-core.js";
const hash = bytes => createHash("sha256").update(bytes).digest("hex");
const source = await readFile(new URL("./worker.js", import.meta.url), "utf8");
assert.equal(hash(source), "739c39cf7bfda382c951f36fc2c9a1b13af32877d3a2bef23bb5cd7a18c2411e");
const startMarker = "function messageHandler(";
const endMarker = "\nfunction extractWasm(";
assert.equal(source.split(startMarker).length, 2);
assert.equal(source.split(endMarker).length, 2);
const start = source.indexOf(startMarker);
const separator = source.indexOf(endMarker, start);
// Include the one LF separator in the RAW pin; never trim source.
const closure = source.slice(start, separator + 1);
assert.equal(hash(closure), "de7987464acb960adc692cf6399c0c70017a082bc38b14df97a9a251ec9dc255");
const core = await readFile(new URL("./statx-core.js", import.meta.url));
assert.equal(hash(core), "3b4ff211ae2b678affba649ccef996c232be231010217f6acce329bdf39cf549");
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
