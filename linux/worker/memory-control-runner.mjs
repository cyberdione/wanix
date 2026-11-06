// Privileged finite controls entry. Runtime modules are supplied only by reviewed packaging.
import {createMemoryControlPairedLoader} from './memory-control-runtime/memory-control-paired-loader.mjs';

const scenarios = Object.freeze(['rw','none','ro','cross-none','host-copy','rw-store-readback','none-load','mprotect-hole-prefix']);
const faults = new Set(['none','ro','cross-none','none-load','mprotect-hole-prefix']);

export async function runMemoryControl(data, caller) {
    // Strict inner-worker identity: outer Go transport must normalize before dispatch.
    if (!data || !Number.isInteger(data.tid) || data.tid <= 0 || data.tid > 2147483647)
        throw new TypeError('positive i32 control task identity required');
    if (data.mode !== 'memory-controls-v1' || data.fixture !== 'scalar-i32-v1' || !scenarios.includes(data.scenario))
        throw new TypeError('fixed control selector required');
    if (Object.keys(data).some(key => !['tid','mode','fixture','scenario'].includes(key)))
        throw new TypeError('unexpected control input');
    const identity = data.tid, scenario = data.scenario;
    let cleanups = 0;
    const loader = createMemoryControlPairedLoader({
        instantiate: (bytes, imports) => WebAssembly.instantiate(bytes, imports),
        cleanup: () => { cleanups++; },
    });
    const task = await loader.load({mode:'memory-controls-v1',fixture:'scalar-i32-v1',scenario});
    const outcome = await task.run();
    const expected = faults.has(scenario) ? 'memory-fault' : 'return';
    if (outcome.kind !== expected || cleanups !== 1 ||
        (expected === 'memory-fault' ? outcome.error?.code !== 'EFAULT' : outcome.value !== (scenario === 'rw-store-readback' ? 99 : 71)))
        throw new Error('unexpected control outcome');
    const report = Object.freeze({tid:identity,scenario,kind:outcome.kind,cleanups,
        ...(expected === 'return' ? {value:outcome.value} : {errno:'EFAULT'})});
    // Existing worker handler: path_open -> numeric fd; fd_write -> count or ioError.
    // CallBuffer.call adds method to params: never pass the frozen report itself.
    const fd = caller.call('path_open', {path:`#task/${identity}/fd/1`});
    if (!Number.isSafeInteger(fd) || fd < 0) throw new Error('invalid report fd');
    let primary;
    try {
        const bytes = new TextEncoder().encode(JSON.stringify(report) + '\n');
        let at = 0;
        while (at < bytes.length) {
            const count = caller.call('fd_write', {fd,data:bytes.slice(at)});
            if (count && typeof count === 'object' && Number.isInteger(count.ioError))
                throw Object.assign(new Error('control report I/O failure'), {errno:count.ioError});
            if (!Number.isSafeInteger(count) || count <= 0 || count > bytes.length - at)
                throw new Error('control report write failed');
            at += count;
        }
    } catch (error) { primary = error; }
    // stdout is a stream: acknowledged writes do not require optional fsync.
    try { caller.call('fd_close', {fd}); }
    catch (error) { if (!primary) primary = error; }
    if (primary) throw primary;
    caller.call('exit', {code:0});
    return report;
}
