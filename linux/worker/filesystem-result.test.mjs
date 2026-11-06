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

globalThis.self = {};
const { messageHandler } = await import("./worker.js");
import { readFile } from "node:fs/promises";

function fixtureDecode(hex) {
    const cb = new CallBuffer(new SharedArrayBuffer(256));
    const previous = globalThis.postMessage;
    globalThis.postMessage = () => {
        const bytes = Buffer.from(hex, "hex");
        cb.data.set(bytes); cb.len[0] = bytes.length;
        Atomics.store(cb.ctrl, 0, 1);
    };
    try { return cb.call("fixture", {}); }
    finally { globalThis.postMessage = previous; }
}

test("Go fixtures through actual JS decoder, explicit handle and broker response codec", async () => {
    const fixtures = JSON.parse(await readFile(new URL("../../api/filesystem_result.fixture.json", import.meta.url)));
    const h = Object.create(WanixHandle.prototype);
    let value = fixtureDecode(fixtures.notExist);
    h.peer = {async call() {return {value};}};
    const cb = new CallBuffer(new SharedArrayBuffer(256));
    const broker = messageHandler(h, cb, "test");
    for (const [envelope, errno] of [[value, 2], [{OK:false, Error:{Code:"EACCES"}},13],
        [{OK:false, Error:{Code:"UNKNOWN"}},5]]) {
        value = envelope;
        cb.ctrl[0] = 0;
        await broker({data:{method:"path_open_existing",path:"#task/test/w/out.txt"}});
        const reply = fixtureDecode(Buffer.from(cb.data.slice(0, cb.len[0])).toString("hex"));
        assert.deepEqual(reply, {ok:false,errno});
    }
    h.peer.call = async () => {throw new Error("no such file: permission denied");};
    await broker({data:{method:"path_stat",path:"#task/test/w/out.txt"}});
    assert.deepEqual(fixtureDecode(Buffer.from(cb.data.slice(0, cb.len[0])).toString("hex")), {ok:false,errno:5});
    const selectors = [];
    h.peer.call = async selector => {
        selectors.push(selector);
        return {value: fixtureDecode(fixtures[selector === "OpenFileResult" ? "openZero" : "statNegative"])};
    };
    await broker({data:{method:"path_open_existing",path:"#task/test/w/out.txt"}});
    assert.deepEqual(fixtureDecode(Buffer.from(cb.data.slice(0, cb.len[0])).toString("hex")), {ok:true,fd:0});
    assert.deepEqual(selectors, ["LstatResult", "OpenFileResult"]);
});

test("real CallBuffer overflow closes unpublished fd exactly once without retry", async () => {
    for (const method of ["path_open_existing", "path_open"]) {
        const closed = [], cb = new CallBuffer(new SharedArrayBuffer(9));
        const broker = messageHandler({
            async lstatResult() {return {OK:true,Value:{Mode:0x8000,IsDir:false}};},
            async openResult() {return {OK:true,Value:1000};},
            async openFileResult() {return {OK:true,Value:1000};},
            async close(fd) {closed.push(fd);},
        }, cb, "test");
        await broker({data:{method,path:"#task/test/w/out.txt"}});
        assert.deepEqual(closed,[1000]);
        assert.equal(Atomics.load(cb.ctrl,0),0);
    }
});

test("consumed response followed by ready reset and throw never reclaims fd", async () => {
    for (const method of ["path_open_existing", "path_open"]) {
        const cb = new CallBuffer(new SharedArrayBuffer(256)), closed = [];
        const respond = cb.respond.bind(cb);
        cb.respond = value => {
            respond(value);
            // Recipient consumed success and started its next synchronous call.
            Atomics.store(cb.ctrl, 0, 0);
            throw new Error("after publication and consumption");
        };
        const broker = messageHandler({
            async lstatResult() {return {OK:true,Value:{Mode:0x8000,IsDir:false}};},
            async openResult() {return {OK:true,Value:7};},
            async openFileResult() {return {OK:true,Value:7};},
            async close(fd) {closed.push(fd);},
        },cb,"test");
        await broker({data:{method,path:"#task/test/w/out.txt"}});
        assert.deepEqual(closed,[]);
        assert.equal(Atomics.load(cb.ctrl,0),0);
        const published = fixtureDecode(Buffer.from(cb.data.slice(0, cb.len[0])).toString("hex"));
        assert.deepEqual(published, method === "path_open" ? 7 : {ok:true,fd:7});
    }
});
