import assert from "node:assert/strict";
import test from "node:test";

import { WanixHandle } from "./handle.js";

function handle(overrides) {
    return Object.assign(Object.create(WanixHandle.prototype), overrides);
}

test("readable EOF closes owned fd without enqueueing null", async () => {
    const closed = [];
    const reader = handle({
        async read() { return null; },
        async close(fd) { closed.push(fd); },
    }).readable(41).getReader();
    assert.deepEqual(await reader.read(), { value: undefined, done: true });
    assert.deepEqual(closed, [41]);
});

test("readable cancellation closes owned fd exactly once", async () => {
    const closed = [];
    const stream = handle({
        async read() { throw new Error("unexpected read"); },
        async close(fd) { closed.push(fd); },
    }).readable(42);
    await stream.cancel("consumer stopped");
    assert.deepEqual(closed, [42]);
});

test("late read result after cancellation is discarded", async () => {
    let resolveRead;
    const closed = [];
    const stream = handle({
        read() { return new Promise(resolve => { resolveRead = resolve; }); },
        async close(fd) { closed.push(fd); },
    }).readable(43);
    const reader = stream.getReader();
    const pending = reader.read();
    await new Promise(resolve => queueMicrotask(resolve));
    const cancelled = reader.cancel();
    resolveRead(new Uint8Array([1]));
    await cancelled;
    assert.deepEqual(closed, [43]);
    await pending;
});

test("writable close and abort close owned fd", async () => {
    const closed = [];
    const h = handle({
        async write() {},
        async close(fd) { closed.push(fd); },
    });
    const writer = h.writable(44).getWriter();
    await writer.write(new Uint8Array([1]));
    await writer.close();
    const aborted = h.writable(45).getWriter();
    await aborted.abort("stop");
    assert.deepEqual(closed, [44, 45]);
});

test("readAt forwards fd, count and offset", async () => {
    const data = new Uint8Array([3, 4]);
    const h = handle({
        logger() {},
        peer: { async call(method, args) {
            assert.equal(method, "ReadAt");
            assert.deepEqual(args, [7, 2, 11]);
            return { value: data };
        } },
    });
    assert.equal(await h.readAt(7, 2, 11), data);
});

import { CBORCodec } from "@progrium/duplex";
import { FilesystemResultProtocolError } from "./handle.js";

async function codecRoundtrip(value) {
    const codec = new CBORCodec();
    let bytes;
    await codec.encoder({ async write(data) { bytes = data.slice(); return data.length; } }).encode(value);
    let offset = 0;
    return codec.decoder({ async read(data) {
        const n = Math.min(data.length, bytes.length - offset);
        data.set(bytes.subarray(offset, offset + n)); offset += n; return n;
    } }).decode(bytes.length);
}

function resultHandle(value, calls = []) {
    return handle({ logger() {}, peer: { async call(method, args) {
        calls.push([method, args]); return { value };
    } } });
}

test("explicit result selectors forward one path and preserve envelopes through real duplex CBOR", async () => {
    for (const [method, selector] of [["openResult", "OpenResult"], ["statResult", "StatResult"], ["lstatResult", "LstatResult"]]) {
        const calls = [];
        const value = await codecRoundtrip({ OK: false, Error: { Code: "EPERM" } });
        assert.equal(await resultHandle(value, calls)[method]("/x"), value);
        assert.deepEqual(calls, [[selector, ["/x"]]]);
    }
    assert.deepEqual(await resultHandle(await codecRoundtrip({ OK: true, Value: 0 })).openResult("/x"), {OK: true, Value: 0});
});

test("malformed result envelopes reject locally; unsafe fds are never closed", async () => {
    for (const value of [null, [], {}, { OK: 1 }, { OK: true }, { OK: false },
        {OK: false, Error: {Code: "UNKNOWN"}}, {OK: false, Error: {Code: "ENOENT"}, Value: 0},
        ...[-1, 1.5, Number.MAX_SAFE_INTEGER + 1, 1n, "0", null].map(Value => ({OK: true, Value}))]) {
        const calls = [];
        await assert.rejects(resultHandle(value, calls).openResult("x"), FilesystemResultProtocolError);
        assert.deepEqual(calls, [["OpenResult", ["x"]]]);
    }
});

test("recognizable allocation rejected before handoff closes once and retains cleanup failure", async () => {
    const calls = [], cleanupError = new Error("close failed");
    const h = resultHandle({OK: true, Value: 7, Error: {Code: "EIO"}}, calls);
    h.close = async fd => { calls.push(["Close", [fd]]); throw cleanupError; };
    await assert.rejects(h.openResult("x"), error => error instanceof FilesystemResultProtocolError && error.cleanupError === cleanupError);
    assert.deepEqual(calls, [["OpenResult", ["x"]], ["Close", [7]]]);
});

test("RPC rejection stays distinct and never retries or closes a guessed fd", async () => {
    const failure = new Error("unsupported selector"), calls = [];
    const h = handle({ peer: { async call(...args) { calls.push(args); throw failure; } } });
    for (const method of ["openResult", "statResult", "lstatResult"]) await assert.rejects(h[method]("x"), error => error === failure);
    assert.equal(calls.length, 3);
});

test("legacy selectors return their original unvalidated values", async () => {
    const calls = [], value = {legacy: true};
    const h = resultHandle(value, calls);
    for (const method of ["open", "stat", "lstat"]) assert.equal(await h[method]("x"), value);
    assert.deepEqual(calls.map(c => c[0]), ["Open", "Stat", "Lstat"]);
});

import { readFile } from "node:fs/promises";

test("actual Go CBOR fixtures decode with duplex codec and validate unchanged", async () => {
    const fixtures = JSON.parse(await readFile(new URL("./filesystem_result.fixture.json", import.meta.url)));
    const decoded = {};
    for (const [name, hex] of Object.entries(fixtures)) {
        const bytes = Buffer.from(hex, "hex"); let offset = 0;
        decoded[name] = await new CBORCodec().decoder({ async read(target) {
            const n = Math.min(target.length, bytes.length - offset);
            target.set(bytes.subarray(offset, offset + n)); offset += n; return n;
        } }).decode(bytes.length);
    }
    assert.deepEqual(await resultHandle(decoded.openZero).openResult("x"), {OK: true, Value: 0});
    assert.deepEqual(await resultHandle(decoded.openZero).openFileResult("x", "read"), {OK: true, Value: 0});
    assert.deepEqual(await resultHandle(decoded.notExist).openFileResult("x", "read"), {OK: false, Error: {Code: "ENOENT"}});
    assert.deepEqual(await resultHandle(decoded.notExist).openResult("x"), {OK: false, Error: {Code: "ENOENT"}});
    for (const method of ["statResult", "lstatResult"]) {
        assert.deepEqual(await resultHandle(decoded.statNegative)[method]("x"), {
            OK: true, Value: {Size: -1, Mode: 33188, IsDir: false, ModTime: 1700000000},
        });
        for (const patch of [{Size: 2 ** 53}, {Size: 1n}, {Mode: -1}, {Mode: 2 ** 32},
            {IsDir: 0}, {ModTime: new Date()}, {ModTime: NaN}, {ModTime: "1700000000"}]) {
            await assert.rejects(resultHandle({OK: true, Value: {...decoded.statNegative.Value, ...patch}})[method]("x"), FilesystemResultProtocolError);
        }
        await assert.rejects(resultHandle({OK: true, Value: {Size: 0, Mode: 0, IsDir: false}})[method]("x"), FilesystemResultProtocolError);
    }
});

test("openFileResult sends the direct six-field semantic map with explicit access", async () => {
    for (const access of ["read", "write", "readwrite"]) {
        const calls = [], value = await codecRoundtrip({OK: true, Value: 0});
        assert.deepEqual(await resultHandle(value, calls).openFileResult("/x", access), value);
        const request = {Path: "/x", Access: access, Create: false, Exclusive: false, Truncate: false, Perm: 0};
        assert.deepEqual(calls, [["OpenFileResult", request]]);
        assert.deepEqual(await codecRoundtrip(calls[0][1]), request);
    }
    const calls = [], value = {OK: false, Error: {Code: "ENOSYS"}};
    assert.equal(await resultHandle(value, calls).openFileResult("/x", "write", {
        create: true, exclusive: true, truncate: true, perm: 0o640,
    }), value);
    assert.deepEqual(calls, [["OpenFileResult", {
        Path: "/x", Access: "write", Create: true, Exclusive: true, Truncate: true, Perm: 0o640,
    }]]);
});

test("openFileResult rejects numeric flags and invalid semantic requests before RPC", async () => {
    const calls = [], h = resultHandle(null, calls);
    for (const args of [["x"], ["x", 0], ["x", "append"], [null, "read"],
        ["x", "read", {truncate: true}], ["x", "write", {exclusive: true}],
        ["x", "write", {perm: 0o600}], ["x", "write", {create: 1}],
        ["x", "write", {create: true, perm: 0o1000}], ["x", "write", {create: true, perm: 1.5}]]) {
        await assert.rejects(h.openFileResult(...args), TypeError);
    }
    assert.deepEqual(calls, []);
});

test("openFileResult malformed or missing replies never close an unrelated fd", async () => {
    for (const value of [undefined, null, [], {}, {OK: true}, {OK: false},
        {OK: true, Value: 7, Error: {Code: "EIO"}},
        {OK: false, Value: 7, Error: {Code: "ENOENT"}},
        {OK: false, Error: {Code: "UNKNOWN"}},
        ...[-1, 1.5, 2 ** 53, "7", 7n].map(Value => ({OK: true, Value}))]) {
        const calls = [];
        await assert.rejects(resultHandle(value, calls).openFileResult("x", "read"), FilesystemResultProtocolError);
        assert.deepEqual(calls.map(c => c[0]), ["OpenFileResult"]);
    }
    const h = handle({peer: {async call() { return undefined; }}});
    await assert.rejects(h.openFileResult("x", "read"), FilesystemResultProtocolError);
});

test("openFileResult RPC failure stays distinct without retry or cleanup", async () => {
    const calls = [], failure = new Error("unsupported selector");
    const h = handle({peer: {async call(...args) { calls.push(args); throw failure; }}});
    await assert.rejects(h.openFileResult("x", "read"), error => error === failure);
    assert.equal(calls.length, 1);
});
