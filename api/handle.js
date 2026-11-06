import * as duplex from "@progrium/duplex";

export class FilesystemResultProtocolError extends Error {
    constructor() {
        super("invalid filesystem result envelope");
        this.name = "FilesystemResultProtocolError";
    }
}

const filesystemCodes = new Set([
    "ENOENT", "EACCES", "EPERM", "EEXIST", "EINVAL", "ENOTDIR",
    "EISDIR", "ELOOP", "ENAMETOOLONG", "EIO", "ENOSYS", "EOVERFLOW",
]);
const record = value => value !== null && typeof value === "object" && !Array.isArray(value);
const owns = (value, key) => Object.prototype.hasOwnProperty.call(value, key);
const validFD = value => Number.isSafeInteger(value) && value >= 0;

function validStat(value) {
    return record(value) && ["Size", "Mode", "IsDir", "ModTime"].every(key => owns(value, key)) &&
        Number.isSafeInteger(value.Size) && Number.isSafeInteger(value.Mode) &&
        value.Mode >= 0 && value.Mode <= 0xffffffff && typeof value.IsDir === "boolean" &&
        typeof value.ModTime === "number" && Number.isFinite(value.ModTime);
}

function validateResult(result, validValue) {
    if (!record(result) || typeof result.OK !== "boolean") throw new FilesystemResultProtocolError();
    if (result.OK) {
        if (!owns(result, "Value") || owns(result, "Error") || !validValue(result.Value)) {
            throw new FilesystemResultProtocolError();
        }
    } else if (owns(result, "Value") || !record(result.Error) || !filesystemCodes.has(result.Error.Code)) {
        throw new FilesystemResultProtocolError();
    }
    return result;
}

export class WanixHandle {
    constructor(port) {
        const sess = new duplex.Session(new duplex.PortConn(port));
        this.peer = new duplex.Peer(sess, new duplex.CBORCodec());
        this.logger = () => null;
    }

    async readDir(name) {
        this.logger(`readDir ${name}`);
        return (await this.peer.call("ReadDir", [name])).value;
    }

    async makeDir(name) {
        this.logger(`makeDir ${name}`);
        await this.peer.call("Mkdir", [name]);
    }

    async makeDirAll(name) {
        this.logger(`makeDirAll ${name}`);
        await this.peer.call("MkdirAll", [name]);
    }

    async bind(name, newname) {
        this.logger(`unbind ${name} ${newname}`);
        await this.peer.call("Bind", [name, newname]);
    }

    async unbind(name, newname) {
        this.logger(`unbind ${name} ${newname}`);
        await this.peer.call("Unbind", [name, newname]);
    }
    
    async readFile(name) {
        this.logger(`readFile ${name}`);
        return (await this.peer.call("ReadFile", [name])).value;
    }

    // not sure if readFile approach is good, but this is an option for now
    async readFile2(name) {
        this.logger(`readFile2 ${name}`);
        const rd = await this.openReadable(name);
        const response = new Response(rd); // cute trick
        return new Uint8Array(await response.arrayBuffer());
    }

    async readText(name) {
        return (new TextDecoder()).decode(await this.readFile(name));
    }

    async waitFor(name, timeoutMs=1000) {
        this.logger(`waitFor ${name} ${timeoutMs}ms`);
        await this.peer.call("WaitFor", [name, timeoutMs]);
    }

    async stat(name) {
        this.logger(`stat ${name}`);
        return (await this.peer.call("Stat", [name])).value;
    }

    async writeFile(name, contents) {
        this.logger(`writeFile ${name} len(${contents.length})`);
        if (typeof contents === "string") {
            contents = (new TextEncoder()).encode(contents);
        }
        return (await this.peer.call("WriteFile", [name, contents])).value;
    }

    async appendFile(name, contents) {
        this.logger(`appendFile ${name} len(${contents.length})`);
        if (typeof contents === "string") {
            contents = (new TextEncoder()).encode(contents);
        }
        return (await this.peer.call("AppendFile", [name, contents])).value;
    }

    async rename(oldname, newname) {
        this.logger(`rename ${oldname} ${newname}`);
        await this.peer.call("Rename", [oldname, newname]);
    }

    async copy(oldname, newname) {
        this.logger(`copy ${oldname} ${newname}`);
        await this.peer.call("Copy", [oldname, newname]);
    }

    async remove(name) {
        this.logger(`remove ${name}`);
        await this.peer.call("Remove", [name]);
    }

    async removeAll(name) {
        this.logger(`removeAll ${name}`);
        await this.peer.call("RemoveAll", [name]);
    }

    async truncate(name, size) {
        this.logger(`truncate ${name} ${size}`);
        await this.peer.call("Truncate", [name, size]);
    }

    async create(name) {
        this.logger(`create ${name}`);
        return (await this.peer.call("Create", [name])).value;
    }

    async open(name) {
        this.logger(`open ${name}`);
        return (await this.peer.call("Open", [name])).value;
    }

    async openResult(name) {
        const result = (await this.peer.call("OpenResult", [name])).value;
        try {
            return validateResult(result, validFD);
        } catch (error) {
            // A recognizable allocation rejected before handoff still needs an owner.
            // Unrecognizable numbers remain task-owned until teardown.
            if (record(result) && result.OK === true && owns(result, "Value") && validFD(result.Value)) {
                try { await this.close(result.Value); }
                catch (cleanupError) { error.cleanupError = cleanupError; }
            }
            throw error;
        }
    }

    async statResult(name) {
        return validateResult((await this.peer.call("StatResult", [name])).value, validStat);
    }

    async lstatResult(name) {
        return validateResult((await this.peer.call("LstatResult", [name])).value, validStat);
    }

    // Semantic access is explicit; guest numeric flags are not portable here.
    async openFileResult(name, access, { create = false, exclusive = false, truncate = false, perm = 0, maxExistingSize } = {}) {
        if (typeof name !== "string" || !["read", "write", "readwrite"].includes(access) ||
            typeof create !== "boolean" || typeof exclusive !== "boolean" || typeof truncate !== "boolean" ||
            !Number.isInteger(perm) || perm < 0 || perm > 0o777 || (!create && perm !== 0) ||
            (exclusive && !create) || (access === "read" && truncate) ||
            (maxExistingSize !== undefined && (!Number.isSafeInteger(maxExistingSize) || maxExistingSize < 0))) {
            throw new TypeError("invalid semantic open parameters");
        }
        const response = await this.peer.call("OpenFileResult", {
            Path: name, Access: access, Create: create, Exclusive: exclusive, Truncate: truncate, Perm: perm,
            ...(maxExistingSize === undefined ? {} : {MaxExistingSize: maxExistingSize}),
        });
        // A malformed reply does not establish ownership of any descriptor.
        // In particular, never close a number guessed from an invalid envelope.
        return validateResult(response?.value, validFD);
    }

    async openFile(name, flags, mode) {
        this.logger(`openFile ${name} ${flags} ${mode}`);
        return (await this.peer.call("OpenFile", [name, flags, mode])).value;
    }

    async read(fd, count) {
        this.logger(`read ${fd} ${count}`);
        return (await this.peer.call("Read", [fd, count])).value;
    }

    async readAt(fd, count, offset) {
        this.logger(`readAt ${fd} ${count} ${offset}`);
        return (await this.peer.call("ReadAt", [fd, count, offset])).value;
    }

    async write(fd, data) {
        this.logger(`write ${fd} len(${data.length})`);
        return (await this.peer.call("Write", [fd, data])).value;
    }

    async writeAt(fd, data, offset) {
        this.logger(`writeAt ${fd} ${offset}`);
        return (await this.peer.call("WriteAt", [fd, data, offset])).value;
    }

    async close(fd) {
        this.logger(`close ${fd}`);
        return (await this.peer.call("Close", [fd])).value;
    }

    async sync(fd) {
        this.logger(`sync ${fd}`);
        return (await this.peer.call("Sync", [fd])).value;
    }

    async fstat(fd) {
        this.logger(`fstat ${fd}`);
        return (await this.peer.call("Fstat", [fd])).value;
    }

    async lstat(name) {
        this.logger(`lstat ${name}`);
        return (await this.peer.call("Lstat", [name])).value;
    }

    async chmod(name, mode) {
        this.logger(`chmod ${name} ${mode}`);
        await this.peer.call("Chmod", [name, mode]);
    }

    async chown(name, uid, gid) {
        this.logger(`chown ${name} ${uid} ${gid}`);
        await this.peer.call("Chown", [name, uid, gid]);
    }

    async fchmod(fd, mode) {
        this.logger(`fchmod ${fd} ${mode}`);
        await this.peer.call("Fchmod", [fd, mode]);
    }

    async fchown(fd, uid, gid) {
        this.logger(`fchown ${fd} ${uid} ${gid}`);
        await this.peer.call("Fchown", [fd, uid, gid]);
    }

    async ftruncate(fd, length) {
        this.logger(`ftruncate ${fd} ${length}`);
        await this.peer.call("Ftruncate", [fd, length]);
    }

    async readlink(name) {
        this.logger(`readlink ${name}`);
        return (await this.peer.call("Readlink", [name])).value;
    }

    async symlink(oldname, newname) {
        this.logger(`symlink ${oldname} ${newname}`);
        await this.peer.call("Symlink", [oldname, newname]);
    }

    async chtimes(name, atime, mtime) {
        this.logger(`chtimes ${name} ${atime} ${mtime}`);
        await this.peer.call("Chtimes", [name, atime, mtime]);
    }

    async openReadable(name) {
        this.logger(`openReadable ${name}`);
        const fd = await this.open(name);
        return this.readable(fd);
    }

    async openWritable(name) {
        this.logger(`openWritable ${name}`);
        const fd = await this.openFile(name, 1, 0);
        return this.writable(fd);
    }

    writable(fd) {
        const self = this;
        let closed = false;
        let closing;
        const closeFD = () => {
            if (!closing) {
                closed = true;
                closing = Promise.resolve().then(() => self.close(fd));
            }
            return closing;
        };
        return new WritableStream({
            write(chunk) {
                if (closed) {
                    throw new TypeError("write to closed Wanix stream");
                }
                return self.write(fd, chunk);
            },
            close: closeFD,
            abort: closeFD,
        });
    }

    readable(fd) {
        const self = this;
        let closed = false;
        let closing;
        const closeFD = () => {
            if (!closing) {
                closed = true;
                // This is the existing local Close RPC. It releases the owned
                // descriptor, but cannot interrupt an already-running Read RPC
                // without a separate, explicitly defined cancellation protocol.
                closing = Promise.resolve().then(() => self.close(fd));
            }
            return closing;
        };
        return new ReadableStream({
            async pull(controller) {
                const data = await self.read(fd, 1024);
                // A cancellation can race an outstanding Read RPC. Discard a
                // late result; never enqueue after a terminal transition.
                if (closed) return;
                if (data === null) {
                    await closeFD();
                    controller.close();
                    return;
                }
                controller.enqueue(data);
            },
            cancel: closeFD,
        });
    }

}

// for safari
if (!ReadableStream.prototype[Symbol.asyncIterator]) {
    ReadableStream.prototype[Symbol.asyncIterator] = async function* () {
        const reader = this.getReader();
        try {
            while (true) {
                const { done, value } = await reader.read();
                if (done) return;
                yield value;
            }
        } finally {
            reader.releaseLock();
        }
    };
}