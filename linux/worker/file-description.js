// A regular-file open description over Wanix positional file operations.
export class FileDescription {
    constructor(handle) { this.handle = handle; this.offset = 0; }
    read(buffer) {
        const n = this.handle.read(buffer, { at: this.offset });
        if (n > 0) this.offset += n;
        return n;
    }
    write(buffer) {
        const n = this.handle.write(buffer, { at: this.offset });
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
