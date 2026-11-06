// Diagnostic transformed-source closure unit; NOT module import or wire coverage.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import vm from "node:vm";
import { exactStatSize } from "./statx-core.js";
const hash = bytes => createHash("sha256").update(bytes).digest("hex");
const source = await readFile(new URL("./worker.js", import.meta.url), "utf8");
assert.equal(hash(source), "59d320862d709cda96b4dd14c76c73240e6845afc1da6f723d527b9151aa15ac");
const startMarker = "function messageHandler(";
const endMarker = "\nfunction extractWasm(";
assert.equal(source.split(startMarker).length, 2);
assert.equal(source.split(endMarker).length, 2);
const start = source.indexOf(startMarker);
const separator = source.indexOf(endMarker, start);
// Include the one LF separator in the RAW pin; never trim source.
const closure = source.slice(start, separator + 1);
assert.equal(hash(closure), "0f510a8fabb105c9918710a4128d1e6edf79ec65d92440b6feab6b94a1cadcd4");
const core = await readFile(new URL("./statx-core.js", import.meta.url));
assert.equal(hash(core), "3c326cd967e23f92100962b42976b26c4c03bcdf34817f06487749f6c169bd6c");
// The closure references module-scope helpers; stubs below satisfy those
// references (the raw pins above bind the real definitions). Host Error
// injection keeps the actual instanceof Error branch agreeing with host fakes.
const context = vm.createContext({
 exactStatSize, Error,
 taskFile: () => true,
 requireRegularLeaf: async () => {},
 filesystemValue: async (fs, method, path) => {
  const result = await fs[method](path);
  if (!result.OK) { const error = new Error("filesystem operation failed"); error.code = result.Error.Code; throw error; }
  return result.Value;
 },
 publishOpen: async (fs, call, fd) => call.respond(fd),
 filesystemErrnos: {EPERM: 1, ENOENT: 2, EIO: 5, EACCES: 13, EEXIST: 17, ENOTDIR: 20, EISDIR: 21, EINVAL: 22, ENAMETOOLONG: 36, ENOSYS: 38, ELOOP: 40, EOVERFLOW: 75},
 ioErrno: () => 5,
 TASKNS: "#task",
});
vm.runInContext(closure + "\nglobalThis.handlerFactory = messageHandler;", context);
const cases = [
 { name: "zero", stat: { Size: 0 }, value: 0 },
 { name: "number", stat: { Size: 37 }, value: 37 },
 { name: "missing size", stat: {}, value: 0 },
 { name: "ENOENT result", result: {OK: false, Error: {Code: "ENOENT"}}, error: "filesystem operation failed" },
 { name: "backend Error", failure: new Error("backend missing"), error: "backend missing" },
 { name: "backend string", failure: "backend denied", error: "backend denied" },
];
for (const c of cases) {
 const calls = [], responses = [];
 const fs = { async statResult(path) { calls.push(path); if ("failure" in c) throw c.failure; if ("result" in c) return c.result; return {OK: true, Value: c.stat}; } };
 const handler = context.handlerFactory(fs, { respond(value) { responses.push(value); } }, "diagnostic");
 await handler({ data: { method: "path_size", path: "file" } });
 assert.deepEqual(calls, ["file"], c.name);
 assert.equal(responses.length, 1, c.name);
 if ("error" in c) {
  assert.equal(responses[0].__wanix_error, c.error, c.name);
  assert.equal(Object.keys(responses[0]).join(","), "__wanix_error", c.name);
 } else assert.equal(responses[0], c.value, c.name);
}
console.log("6 actual producer closure cases passed (transformed-source unit only)");
