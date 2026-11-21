import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Worker } from 'node:worker_threads';

// K3: the CallBuffer transport between the sync worker and the linux task
// worker's messageHandler. K2 bounded the syscall layer against thrown
// RPCs using fake callers; the real error path — the task worker answering
// {__wanix_error} through the shared SAB slot via the real CallBuffer —
// was never exercised. These tests drive the real pair end to end: the
// real CallBuffer caller in a worker thread, the real CallBuffer responder,
// and the real messageHandler from worker.js over one SharedArrayBuffer.
//
// Contract audited and pinned here:
//   1. an error response resolves EXACTLY its pending call (one throw on
//      the sync side) and leaves correlation intact: every later call
//      resolves with its own response, never a stale or shifted one;
//   2. a response larger than the slot must overflow loudly instead of
//      wedging or losing bytes: the handler caps fd_read to the slot so a
//      large read is a legal short read (wasi parity).

globalThis.self = {};
const { messageHandler } = await import('./worker.js');
const { CallBuffer } = await import('./lib.js');

const here = new URL('.', import.meta.url).href;

// The sync-worker role: the real CallBuffer, postMessage shimmed to the
// port like the browser dedicated worker's postMessage-to-parent. Runs a
// scripted sequence and reports every result or error.
const syncRunner = `
import { parentPort, workerData } from 'node:worker_threads';
globalThis.postMessage = (m) => parentPort.postMessage(m);
const { CallBuffer } = await import(workerData.libUrl);
const caller = new CallBuffer(workerData.buffer);
const trace = [];
for (const step of workerData.steps) {
    try {
        const value = caller.call(step.method, structuredClone(step.params));
        trace.push({ method: step.method, value });
    } catch (err) {
        trace.push({ method: step.method, error: err.message });
    }
}
parentPort.postMessage(trace);
`;

const FD = 100;
const CHUNK = 32;
const TOTAL = 34000; // larger than two full-slot reads (2 x 16360)

// The fs side of the real messageHandler: a positioned byte ledger with
// the same call shapes WanixHandle serves. errAt makes every write from
// that sequence number on fail closed, like a wsys slot teardown under
// the task; reads before that still succeed.
function makeFs(errAt, fill) {
    const bytes = new Uint8Array(TOTAL);
    if (fill === 'pattern') for (let i = 0; i < TOTAL; i++) bytes[i] = i & 0x7f;
    let writeSeq = 0;
    let closed = false;
    return {
        async open(path) { assert.equal(path, 'mnt/wsys/1/cons'); return FD; },
        async readAt(fd, count, at) { return bytes.slice(at, at + count); },
        async writeAt(fd, data, at) {
            if (errAt >= 0 && writeSeq++ >= errAt) { closed = true; throw new Error('wsys: no such window id'); }
            assert.equal(fd, FD);
            for (let i = 0; i < data.length; i++) bytes[at + i] = data[i];
            return data.length;
        },
        async writeFile(path, contents) { assert.equal(String(contents), '0'); },
        ledger() { return bytes.slice(); },
        closed() { return closed; },
    };
}

async function withTransport(steps, { errAt = -1, fill } = {}) {
    const libUrl = new URL('lib.js', here).href;
    const buffer = new SharedArrayBuffer(16384);
    const fs = makeFs(errAt, fill);
    const call = new CallBuffer(buffer);
    const sync = new Worker(syncRunner, { eval: true, workerData: { buffer, libUrl, steps } });
    const onMessage = messageHandler(fs, call, 'test');
    try {
        const done = new Promise((res, rej) => {
            sync.on('message', (m) => {
                if (m && m.method) { onMessage({ data: m }); return; }
                res(m);
            });
            sync.on('error', rej);
        });
        const trace = await Promise.race([
            done,
            new Promise((_, rej) => setTimeout(() => rej(new Error('CallBuffer transport wedged')), 10_000)),
        ]);
        return { trace, fs };
    } finally {
        await sync.terminate();
    }
}

test('an errored fd_write resolves exactly its call and every later write reports the slot error; earlier bytes land exactly', async () => {
    const N = 200, ERR_AT = 50, MSG = 9;
    const steps = [{ method: 'path_open', params: { path: 'mnt/wsys/1/cons' } }];
    for (let i = 0; i < N; i++) {
        steps.push({ method: 'fd_write', params: { fd: FD, data: new TextEncoder().encode(('w' + i + ':').padEnd(MSG, '.')), at: i * MSG } });
    }
    const { trace } = await withTransport(steps, { errAt: ERR_AT });

    assert.equal(trace.length, N + 1);
    assert.equal(trace[0].value, FD, 'path_open resolves with the device fd');

    let firstError = -1;
    for (let i = 0; i < N; i++) {
        const t = trace[1 + i];
        if (t.error !== undefined) {
            if (firstError < 0) firstError = i;
            assert.equal(t.error, 'wsys: no such window id', `write ${i} must resolve with the slot error, got ${JSON.stringify(t)}`);
        } else {
            assert.equal(t.value, MSG, `write ${i} resolved with ${JSON.stringify(t)}`);
        }
    }
    // the slot closed under exactly the write the error was injected at:
    // no earlier write consumed the error, no later write resolved with a
    // stale success
    assert.equal(firstError, ERR_AT);
});

test('long-lived interleaving: writes and reads keep resolving with their own responses across a mid-stream slot error', async () => {
    const N = 60, ERR_AT = 30, MSG = 9;
    const steps = [{ method: 'path_open', params: { path: 'mnt/wsys/1/cons' } }];
    for (let i = 0; i < N; i++) {
        steps.push({ method: 'fd_write', params: { fd: FD, data: new TextEncoder().encode(('w' + i + ':').padEnd(MSG, '.')), at: i * MSG } });
        if (i % 7 === 6) steps.push({ method: 'fd_read', params: { fd: FD, count: 64, at: i * MSG } });
    }
    steps.push({ method: 'exit', params: { code: 0 } });
    const { trace, fs } = await withTransport(steps, { errAt: ERR_AT });

    for (const [idx, t] of trace.entries()) {
        const step = steps[idx];
        if (step.method === 'fd_write') {
            if (t.error !== undefined) assert.equal(t.error, 'wsys: no such window id');
            else assert.equal(t.value, step.params.data.length);
        } else if (step.method === 'fd_read') {
            assert.ok(t.error !== undefined || t.value !== undefined, `read ${idx} resolved empty: ${JSON.stringify(t)}`);
        } else if (step.method === 'exit') {
            assert.equal(t.value, true);
        }
    }
    assert.equal(trace[trace.length - 1].value, true, 'exit resolves last, with its own response');
    // byte-exact: every write before the slot error landed its own payload
    // at its own offset — no lost bytes, no shifted or duplicated payloads
    const ledger = fs.ledger();
    for (let i = 0; i < ERR_AT; i++) {
        const want = new TextEncoder().encode(('w' + i + ':').padEnd(MSG, '.'));
        assert.deepEqual([...ledger.subarray(i * MSG, i * MSG + MSG)], [...want], `write ${i} bytes absent or corrupted`);
    }
    for (let i = ERR_AT; i < N; i++) {
        for (const b of ledger.subarray(i * MSG, i * MSG + MSG)) assert.equal(b, 0, `write ${i} must not land after the slot error`);
    }
});

test('fd_read is capped to the response slot: an oversized read is a short read, not an overflow error', async () => {
    // 20000 requested bytes cannot fit the 16384-byte slot (16376 data
    // bytes). The wasi personality caps fd_read to call.maxData for exactly
    // this reason; the linux handler must do the same, or respond()
    // overflows, the handler error-path fails the whole read, and the
    // device bytes the oversized read consumed are lost.
    const cap = 16384 - 8 - 16;
    const steps = [
        { method: 'path_open', params: { path: 'mnt/wsys/1/cons' } },
        { method: 'fd_read', params: { fd: FD, count: 20000, at: 0 } },
        { method: 'fd_read', params: { fd: FD, count: cap, at: cap } },
    ];
    const { trace } = await withTransport(steps, { fill: 'pattern' });

    const big = trace[1];
    assert.equal(big.error, undefined, `oversized read must deliver a short read, got ${JSON.stringify(big)}`);
    assert.ok(big.value instanceof Uint8Array);
    assert.equal(big.value.length, cap, 'read delivers the slot capacity as a legal short read');
    assert.equal(big.value[0], 0, 'short read carries the exact bytes at the offset');
    // the follow-up read at the next offset proves no position disturbance
    const next = trace[2];
    assert.equal(next.error, undefined, `follow-up read must deliver, got ${JSON.stringify(next)}`);
    assert.equal(next.value.length, cap);
    assert.equal(next.value[0], cap & 0x7f);
});
