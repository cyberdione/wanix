import assert from "node:assert/strict";
import test from "node:test";
import { CallBuffer, WanixHandle } from "./lib.js";

test("generated explicit handle methods survive bundling", async () => {
    const handle = Object.create(WanixHandle.prototype);
    handle.peer = {async call(selector, args) {
        assert.equal(selector, "OpenResult"); assert.deepEqual(args, ["x"]);
        return {value: {OK: true, Value: 0}};
    }};
    assert.deepEqual(await handle.openResult("x"), {OK: true, Value: 0});
});

test("CallBuffer preserves ordinary broker failure envelopes through its real codec", () => {
    const buffer = new SharedArrayBuffer(256), caller = new CallBuffer(buffer), responder = new CallBuffer(buffer);
    const original = globalThis.postMessage;
    const failure = {ok: false, errno: 13};
    globalThis.postMessage = request => {
        assert.deepEqual(request, {path: "x", method: "path_open_existing"});
        responder.respond(failure);
    };
    try { assert.deepEqual(caller.call("path_open_existing", {path: "x"}), failure); }
    finally { globalThis.postMessage = original; }
});

test("CallBuffer publication overflow throws before notifying or writing response state", () => {
    const buffer = new SharedArrayBuffer(32), responder = new CallBuffer(buffer);
    const before = new Uint8Array(buffer).slice();
    assert.throws(() => responder.respond({ok: true, payload: "x".repeat(100)}), /exceeds CallBuffer/);
    assert.deepEqual(new Uint8Array(buffer), before);
});
