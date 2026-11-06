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
