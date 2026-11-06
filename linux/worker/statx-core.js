// Finite diagnostic repair only; flag/path semantics remain inherited and open.
export function exactStatSize(stat) {
    if (!stat || typeof stat !== "object") throw new Error("missing stat metadata");
    const size = stat.Size;
    if (typeof size === "bigint") {
        if (size < 0n || size > BigInt(Number.MAX_SAFE_INTEGER)) throw new Error("stat size overflow");
        return Number(size);
    }
    if (typeof size !== "number" || !Number.isSafeInteger(size) || size < 0) throw new Error("invalid stat size");
    return size;
}

export function copySizeStatx(memory, pointer, size) {
    // Local EIO mapping is this repair's narrow malformed-success policy.
    if (typeof size !== "number" || !Number.isSafeInteger(size) || size < 0) return -5;
    const record = new Uint8Array(256);
    const view = new DataView(record.buffer);
    view.setUint32(0, 0x200, true);
    view.setBigUint64(40, BigInt(size), true);
    const destination = new Uint8Array(memory.buffer);
    const start = pointer >>> 0;
    if (start > destination.length || record.length > destination.length - start) return -14;
    destination.set(record, start);
    return 0;
}

// Backend FD wire contract: preserve Number major0 integer, never Size coercion.
export function validBackendFD(fd) {
    return typeof fd === "number" && Number.isInteger(fd) && fd >= 0 && fd <= 0xffffffff;
}

export function fdStatSize(guestFD, openFiles, streams, caller, syntheticStdin) {
    if (guestFD === -100 || (guestFD === 0 && syntheticStdin && !streams[0])) return -95;
    const description = openFiles.get(guestFD);
    const handle = description ? description.handle : streams[guestFD];
    if (!handle) return -9;
    if (!validBackendFD(handle.fd)) return -5;
    return fdSizeReply(caller.call("fd_stat_size", {fd:handle.fd}));
}

const filesystemErrnos = {EPERM:1,ENOENT:2,EIO:5,EBADF:9,EACCES:13,EEXIST:17,ENOTDIR:20,EISDIR:21,EINVAL:22,ENAMETOOLONG:36,ELOOP:40};
export function fstatBrokerResult(result) {
    if (result?.OK === false && !Object.hasOwn(result,"Value") && result.Error && Object.hasOwn(filesystemErrnos,result.Error.Code)) return {ok:false,errno:filesystemErrnos[result.Error.Code]};
    if (result?.OK === true && !Object.hasOwn(result,"Error") && result.Value) return {ok:true,size:exactStatSize(result.Value)};
    return {ok:false,errno:5};
}
export function fdSizeReply(reply) {
    if (reply?.ok === false && !Object.hasOwn(reply,"size") && Number.isInteger(reply.errno) && Object.values(filesystemErrnos).includes(reply.errno)) return -reply.errno;
    if (reply?.ok === true && !Object.hasOwn(reply,"errno") && typeof reply.size === "number" && Number.isSafeInteger(reply.size) && reply.size >= 0) return reply.size;
    return -5;
}
