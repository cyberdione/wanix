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
