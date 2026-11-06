// A regular-file open description over Wanix positional file operations.
export class FileDescription {
    // Access modes from asm-generic fcntl.h: RDONLY=0, WRONLY=1, RDWR=2.
    constructor(handle, accessMode) {
        this.handle = handle; this.offset = 0; this.accessMode = accessMode;
    }
    canRead() { return this.accessMode === 0 || this.accessMode === 2; }
    canWrite() { return this.accessMode === 1 || this.accessMode === 2; }
    read(buffer) { return this.transfer('read', buffer); }
    write(buffer) { return this.transfer('write', buffer); }
    transfer(operation, buffer) {
        if (operation === 'read' ? !this.canRead() : !this.canWrite()) return -9;
        // The positional backend admits only exact, nonnegative Number offsets.
        const count = buffer.byteLength;
        if (!Number.isSafeInteger(this.offset) || this.offset < 0 ||
            !Number.isSafeInteger(count) || count < 0 ||
            count > Number.MAX_SAFE_INTEGER - this.offset) return -75;
        const n = this.handle[operation](buffer, { at: this.offset });
        // Do not turn a malformed backend result into a count or cursor update.
        if (!Number.isSafeInteger(n) || n > count) return -5;
        if (n > 0) this.offset += n;
        return n;
    }
    seek(offset, whence) {
        if (whence < 0 || whence > 2) return -22;
        const base = whence === 0 ? 0 : whence === 1 ? this.offset : this.handle.getSize();
        if (base < 0) return base;
        const next = base + offset;
        if (next < 0) return -22;
        if (!Number.isSafeInteger(next)) return -75;
        this.offset = next;
        return next;
    }
    close() { return this.handle.close(); }
}
