import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { CBORCodec } from "@progrium/duplex";
import { WanixHandle } from "../../api/handle.js";

const fixtures = [];
async function encode(value) {
  const chunks = [];
  await new CBORCodec().encoder({ async write(data) {
    chunks.push(Buffer.from(data));
    return data.length;
  } }).encode(value);
  return Buffer.concat(chunks).toString("hex");
}
const handle = Object.create(WanixHandle.prototype);
let calls = 0;
let lastRequest;
handle.peer = { async call(method, request) {
  calls++;
  assert.equal(method, "OpenFileResult");
  lastRequest = request;
  return { value: { OK: true, Value: 7 } };
} };
for (const [Name, limit] of [["six", undefined], ["zero", 0], ["31bit", 2147483647], ["over31bit", 2147483648], ["maxsafe", Number.MAX_SAFE_INTEGER]]) {
  await handle.openFileResult("/out", "write", { create: true, truncate: true, perm: 420, maxExistingSize: limit });
  assert.equal(typeof lastRequest.MaxExistingSize, limit === undefined ? "undefined" : "bigint");
  fixtures.push({ Name, Hex: await encode(lastRequest), Value: limit ?? 0, Present: limit !== undefined });
}
// Reproduce the regression: the same safe public Number is encoded as float64.
const floatRequest = { ...lastRequest, MaxExistingSize: Number.MAX_SAFE_INTEGER };
const floatHex = await encode(floatRequest);
assert.ok(floatHex.endsWith("fb433fffffffffffff"), floatHex);
assert.ok(fixtures.at(-1).Hex.endsWith("1b001fffffffffffff"), fixtures.at(-1).Hex);
fixtures.push({ Name: "maxsafe-number-rejected", Hex: floatHex, Value: 0, Present: true, Reject: true });
for (const maxExistingSize of [-1, 0.5, Number.MAX_SAFE_INTEGER + 1, NaN, Infinity, 1n]) {
  const before = calls;
  await assert.rejects(handle.openFileResult("/out", "write", { maxExistingSize }), TypeError);
  assert.equal(calls, before, "invalid input reached RPC");
}
const result = spawnSync("go", ["run", "test/wasi/fixtures/bounded_open_cbor.go"], {
  cwd: fileURLToPath(new URL("../../", import.meta.url)),
  env: { ...process.env, GOPROXY: "off", GOSUMDB: "off" },
  input: JSON.stringify(fixtures), encoding: "utf8",
});
assert.equal(result.status, 0, result.stderr || result.error?.message);
console.log(result.stdout.trim());
console.log("bounded open CBOR interop: passed; invalid inputs rejected before RPC");
