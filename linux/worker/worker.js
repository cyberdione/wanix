import { WanixHandle, CallBuffer, FileHandle } from "./lib.js";
import { FileDescription } from "./file-description.js";

// Linux-personality task worker for Wanix. Two roles, like the wasi worker:
//  1) task worker: receives {worker:{port,tid,url}}, reads cmd/env/bin from the
//     task namespace, spawns the sync worker, and answers its fs calls.
//  2) sync worker: receives {buffer,bin,args,env,stdin,stdout,stderr} and runs
//     the wasm32 artifact with the ferrule.linux.syscall import.

const TASKNS = "#task";

// split a cmd line on spaces, honoring single/double quotes (mirrors the
// wanix task element's env parsing) so `rc -c 'echo hi'` keeps one argument
function splitCmdQuoted(line) {
    const out = [];
    let cur = "", q = null;
    for (const ch of line) {
        if (q) { if (ch === q) q = null; else cur += ch; }
        else if (ch === "'" || ch === '"') q = ch;
        else if (ch === " ") { if (cur) { out.push(cur); cur = ""; } }
        else cur += ch;
    }
    if (cur) out.push(cur);
    return out;
}

self.onmessage = async (e) => {
    if (e.data.worker) {
        console.log("linux task worker started");
        try {
            await initializeTaskWorker(e);
        } catch (err) {
            console.error(`linux task ${e.data.worker.tid} startup failed:`, err);
            await reportTaskFailure(new WanixHandle(e.data.worker.port), e.data.worker.tid, err);
        }
    } else if (e.data.buffer) {
        console.log("linux sync worker started");
        const caller = new CallBuffer(e.data.buffer);
        try {
            await runLinux(e, caller);
        } catch (err) {
            console.error("linux sync worker failed:", err);
            try { caller.call("exit", { code: 1 }); } catch (_) {}
        }
    }
}

async function reportTaskFailure(fs, tid, err) {
    console.error(`linux task ${tid} failed:`, err);
    try { await fs.writeFile(`${TASKNS}/${tid}/exit`, "1"); } catch (_) {}
}

async function initializeTaskWorker(e) {
    const fs = new WanixHandle(e.data.worker.port);
    const tid = e.data.worker.tid;
    const env = (await fs.readText(`${TASKNS}/${tid}/env`)).trim().split("\n").filter(line => line.includes("="));
    const args = splitCmdQuoted((await fs.readText(`${TASKNS}/${tid}/cmd`)).trim());
    let bin;
    try {
        bin = await fs.readFile(args[0]);
    } catch (err) {
        await reportTaskFailure(fs, tid, err);
        return;
    }
    let stdinText = null;
    try { stdinText = await fs.readText(`${TASKNS}/${tid}/w/stdin.txt`); } catch (_) {}
    const buffer = new SharedArrayBuffer(16384);
    const call = new CallBuffer(buffer);
    const worker = new Worker(e.data.worker.url, {type: "module"});
    worker.onmessage = messageHandler(fs, call, tid);
    worker.onerror = (err) => { void reportTaskFailure(fs, tid, err); };
    worker.onmessageerror = (err) => { void reportTaskFailure(fs, tid, err); };
    worker.postMessage({
        buffer,
        bin,
        args,
        env,
        stdin: `${TASKNS}/${tid}/fd/0`,
        stdout: `${TASKNS}/${tid}/fd/1`,
        stderr: `${TASKNS}/${tid}/fd/2`,
        tid,
        stdinText,
    });
}

export function messageHandler(fs, call, tid) {
    return async (e) => {
        if (!e.data.method) return;
        try {
            switch (e.data.method) {
            case "path_open": {
                const fd = await fs.open(e.data.path);
                call.respond(fd);
                break;
            }
            case "path_size": {
                const stat = await fs.stat(e.data.path);
                call.respond(stat?.Size ?? 0);
                break;
            }
            case "path_stat": {
                // Use a typed envelope: CallBuffer throws for __wanix_error.
                try {
                    const prefix = `${TASKNS}/${tid}/w/`;
                    const name = typeof e.data.path === "string" && e.data.path.startsWith(prefix)
                        ? e.data.path.slice(prefix.length) : "";
                    if (!/^(?:out\.txt|tmpfile_[^/]+)$/.test(name)) {
                        call.respond({ok: false, errno: 13});
                        break;
                    }
                    const stat = e.data.nofollow ? await fs.lstat(e.data.path) : await fs.stat(e.data.path);
                    call.respond({ok: true, stat});
                } catch (err) {
                    const message = err instanceof Error ? err.message : String(err);
                    const errno = /file does not exist|no such file/i.test(message) ? 2
                        : /permission denied/i.test(message) ? 13
                        : /not a directory/i.test(message) ? 20 : 5;
                    call.respond({ok: false, errno});
                }
                break;
            }
            case "path_remove": {
                // The RPC exposes generic Remove, not atomic unlink. Admit only
                // regular files; never use RemoveAll or follow a symlink here.
                try {
                    const prefix = `${TASKNS}/${tid}/w/`;
                    const name = typeof e.data.path === "string" && e.data.path.startsWith(prefix)
                        ? e.data.path.slice(prefix.length) : "";
                    if ((!name.startsWith("tmpfile_") && name !== "out.txt") || name.includes("/")) {
                        call.respond({ok: false, message: "permission denied", errno: 13});
                        break;
                    }
                    const stat = await fs.lstat(e.data.path);
                    if (stat.IsDir || (stat.Mode & 0xf000) === 0x4000) {
                        call.respond({ok: false, message: "is a directory", errno: 21});
                    } else if ((stat.Mode & 0xf000) !== 0x8000) {
                        call.respond({ok: false, message: "unsupported file type", errno: 38});
                    } else {
                        await fs.remove(e.data.path);
                        call.respond({ok: true});
                    }
                } catch (err) {
                    const message = err instanceof Error ? err.message : String(err);
                    const errno = /file does not exist|no such file/i.test(message) ? 2
                        : /permission denied/i.test(message) ? 13
                        : /not a directory/i.test(message) ? 20
                        : /directory not empty/i.test(message) ? 39
                        : /operation not supported/i.test(message) ? 38 : 5;
                    call.respond({ok: false, message: message, errno});
                }
                break;
            }
            case "path_touch":
                await fs.writeFile(e.data.path, "");
                call.respond(true);
                break;
            case "fd_close":
                await fs.close(e.data.fd);
                call.respond(true);
                break;
            case "fd_flush":
                await fs.sync(e.data.fd);
                call.respond(true);
                break;
            case "fd_read": {
                const data = e.data.at === undefined
                    ? await fs.read(e.data.fd, e.data.count)
                    : await fs.readAt(e.data.fd, e.data.count, e.data.at);
                call.respond(data);
                break;
            }
            case "fd_write": {
                const wn = e.data.at === undefined
                    ? await fs.write(e.data.fd, e.data.data)
                    : await fs.writeAt(e.data.fd, e.data.data, e.data.at);
                call.respond(wn);
                break;
            }
            case "exit":
                await fs.writeFile(`${TASKNS}/${tid}/exit`, e.data.code.toString());
                call.respond(true);
                break;
            default:
                call.respond({__wanix_error: `unsupported method: ${e.data.method}`});
            }
        } catch (err) {
            call.respond({__wanix_error: err instanceof Error ? err.message : String(err)});
        }
    };
}

function extractWasm(bin) {
    const b0 = new Uint8Array(bin);
    // production payloads are raw wasm modules; the private ELF envelope is
    // also accepted for diagnostic artifacts
    if (b0.length > 8 && b0[0] === 0x00 && b0[1] === 0x61 && b0[2] === 0x73 && b0[3] === 0x6d) {
        return b0;
    }
    const b = b0;
    const hex = (bb, o, n) => Array.from(bb.subarray(o, o + n)).map(x => x.toString(16).padStart(2, "0")).join("");
    const u16 = o => (b[o] | (b[o + 1] << 8)) >>> 0;
    const u32 = o => (b[o] | (b[o + 1] << 8) | (b[o + 2] << 16) | (b[o + 3] << 24)) >>> 0;
    if (b.length < 84 || b.length > 1024 * 1024 || hex(b, 0, 4) !== "7f454c46" || b[4] !== 1 || b[5] !== 1
        || u16(16) !== 2 || u16(18) !== 0xf3fe || u32(24) !== 0 || u32(28) !== 52
        || u16(40) !== 52 || u16(42) !== 32 || u16(44) !== 2) {
        throw new Error("not a supported linux artifact");
    }
    let wasm = null;
    for (let i = 0; i < 2; i++) {
        const off = 52 + i * 32, type = u32(off), at = u32(off + 4), filesz = u32(off + 16), memsz = u32(off + 20);
        if (at > b.length || filesz > b.length - at || memsz !== filesz) throw new Error("segment outside artifact");
        if (type === 1 && !wasm) wasm = b.subarray(at, at + filesz);
    }
    if (!wasm || hex(wasm, 0, 8) !== "0061736d01000000") throw new Error("no wasm payload");
    return wasm;
}

function buildInitialStack(memory, base, capacity, argv, envp) {
    const enc = s => Array.from(new TextEncoder().encode(s + "\0"));
    const strings = [...argv, ...envp].map(enc);
    const head = 4 * (1 + argv.length + 1 + envp.length + 1 + 4);
    const total = head + strings.reduce((n, s) => n + s.length, 0);
    if (total > capacity) throw new Error("initial stack overflow");
    const img = new Uint8Array(total), v = new DataView(img.buffer);
    let w = 0, c = head, i = 0;
    const put = x => { v.setUint32(w, x, true); w += 4; };
    const putStr = () => { const s = strings[i++]; put(base + c); img.set(s, c); c += s.length; };
    put(argv.length); argv.forEach(putStr); put(0);
    envp.forEach(putStr); put(0);
    put(6); put(4096); // AT_PAGESZ
    put(0); put(0);    // AT_NULL
    new Uint8Array(memory.buffer, base, total).set(img);
    return base;
}

export async function runLinux(e, caller) {
    const tid = e.data.tid;
    const stdinBytes = e.data.stdinText == null ? null : new TextEncoder().encode(e.data.stdinText);
    let stdinPos = 0;
    const wasm = extractWasm(e.data.bin);
    const argv = e.data.args, envp = e.data.env;
    const memoryBytes = 16 * 1024 * 1024;
    const streams = {};
    for (const [fd, path] of [[0, e.data.stdin], [1, e.data.stdout], [2, e.data.stderr]]) {
        if (!path) continue;
        const h = new FileHandle(caller, path);
        h.open();
        streams[fd] = h;
    }
    const openFiles = new Map();
    let nextFd = 3;
    let exitCode = 0;
    let inst;
    let arenaNext; // Monotonic, nonreclaiming; starts beyond the startup reservation.

    function syscall(nr, a0, a1, a2, a3, a4, a5) {
        const a = [a0 | 0, a1 | 0, a2 | 0, a3 | 0, a4 | 0, a5 | 0];
        const u = () => new Uint8Array(inst.exports.memory.buffer);
        const v = () => new DataView(inst.exports.memory.buffer);
        const __r = __syscall(nr, a0, a1, a2, a3, a4, a5);
        return __r;
    }
    function __syscall(nr, a0, a1, a2, a3, a4, a5) {
        const a = [a0 | 0, a1 | 0, a2 | 0, a3 | 0, a4 | 0, a5 | 0];
        const u = () => new Uint8Array(inst.exports.memory.buffer);
        const v = () => new DataView(inst.exports.memory.buffer);
        switch (nr) {
        case 66: { // writev(fd, iov, iovcnt)
            const fd = a[0], iov = a[1] >>> 0, cnt = a[2] | 0;
            const vv = v(), uu = u();
            if (cnt > 1024 || iov > uu.length || cnt * 8 > uu.length - iov) return -14;
            let total = 0;
            const parts = [];
            for (let i = 0; i < cnt; i++) {
                const base = vv.getUint32(iov + 8 * i, true), len = vv.getUint32(iov + 8 * i + 4, true);
                if (base > uu.length || len > uu.length - base) return -14;
                const chunk = uu.subarray(base, base + len);
                parts.push(chunk); total += chunk.length;
            }
            const data = parts.length === 1 ? parts[0] : (() => {
                const out = new Uint8Array(total); let o = 0;
                for (const p of parts) { out.set(p, o); o += p.length; }
                return out;
            })();
            const handle = streams[fd] ?? openFiles.get(fd);
            if (!handle) return -9;
            const n = handle.write(data);
            return n ?? 0;
        }
        case 65: { // readv: sequential reads into the supplied vectors
            const fd = a[0], iov = a[1] >>> 0, cnt = a[2] | 0;
            const vv = v(), uu = u();
            if (cnt < 0 || cnt > 1024) return -22;
            if (iov > uu.length || cnt * 8 > uu.length - iov) return -14;
            const vectors = [];
            for (let i = 0; i < cnt; i++) {
                const base = vv.getUint32(iov + 8 * i, true);
                const len = vv.getUint32(iov + 8 * i + 4, true);
                if (base > uu.length || len > uu.length - base) return -14;
                vectors.push([base, len]);
            }
            if (!(streams[fd] ?? openFiles.get(fd)) && !(fd === 0 && stdinBytes !== null)) return -9;
            let total = 0;
            for (const [base, len] of vectors) {
                if (!len) continue;
                const n = __syscall(63, fd, base, len, 0, 0, 0);
                if (n < 0) return total || n;
                total += n;
                if (n < len) break;
            }
            return total;
        }
        case 64: { // write(fd, buf, count)
            const fd = a[0], buf = a[1] >>> 0, count = a[2] >>> 0, uu = u();
            if (buf > uu.length || count > uu.length - buf) return -14;
            const handle = streams[fd] ?? openFiles.get(fd);
            if (!handle) return -9;
            return handle.write(uu.subarray(buf, buf + count)) ?? 0;
        }
        case 56: { // openat(dirfd, path, flags, mode)
            if (a[0] !== -100) return -9;
            const uu = u(); let end = a[1] >>> 0;
            while (end < uu.length && uu[end] !== 0) end++;
            const raw = new TextDecoder().decode(uu.subarray(a[1] >>> 0, end));
            const p = raw.replace(/^\/+/, "");
            if (p === "" || p.split("/").some(seg => seg === ".." || seg === "")) return -13;
            // capability restriction: only the pre-authorized scratch file
            const scratch = p.startsWith("tmp/tmpfile_") || p.startsWith("tmpfile_");
            if (p !== "out.txt" && !scratch) return -13;
            const target = `${TASKNS}/${tid}/w/${scratch ? p.split("/").pop() : p}`;
            const created = caller.call("path_touch", { path: target });
            if (created && created.__wanix_error) return -13;
            const handle = new FileHandle(caller, target);
            try { handle.open(); } catch (_) { return -13; }
            const fd = nextFd++;
            openFiles.set(fd, new FileDescription(handle));
            return fd;
        }
        case 63: { // read(fd, buf, count) — task-provided stdin text or streams
            const fd = a[0], buf = a[1] >>> 0, count = a[2] >>> 0, uu = u();
            if (buf > uu.length || count > uu.length - buf) return -14;
            if (fd === 0 && stdinBytes !== null) {
                const chunk = stdinBytes.subarray(stdinPos, stdinPos + count);
                stdinPos += chunk.length;
                uu.set(chunk, buf);
                return chunk.length; // bytes read; 0 at EOF or for count == 0
            }
            const h = streams[fd] ?? openFiles.get(fd);
            if (!h) return -9;
            const data = new Uint8Array(count);
            const n = h.read(data);
            if (n > 0) uu.set(data.subarray(0, n), buf);
            return n;
        }
        case 57: { const h = openFiles.get(a[0]); if (!h) return -9; h.close(); openFiles.delete(a[0]); return 0; }
        case 62: { // _llseek(fd, off_hi, off_lo, result_ptr, whence) — open files only
            const fd = a[0] | 0, off = (a[1] * 4294967296) + (a[2] >>> 0), res = a[3] >>> 0, whence = a[4] | 0;
            const h = openFiles.get(fd);
            if (!h) return -9;
            if (whence < 0 || whence > 2) return -22;
            const uu = u(), vv = v();
            if (res + 8 > uu.length) return -14;
            const pos = h.seek(off, whence);
            if (pos < 0) return pos;
            vv.setBigUint64(res, BigInt(pos), true);
            return 0;
        }
        case 291: { // statx: task-local regular files, TYPE and SIZE only
            const flags = a[2] >>> 0, mask = a[3] >>> 0;
            if ((flags & ~0x7900) || (flags & 0x6000) === 0x6000 || (mask & 0x80000000)) return -22;
            // Empty-path fd lookup and forced/cache-only sync are not implemented.
            if (flags & 0x7000) return -38;
            const uu = u(), buf = a[4] >>> 0, start = a[1] >>> 0;
            if (!buf || buf > uu.length || 256 > uu.length - buf || !start || start >= uu.length) return -14;
            let end = start;
            while (end < uu.length && uu[end] !== 0) end++;
            if (end === uu.length) return -14;
            const raw = new TextDecoder().decode(uu.subarray(start, end));
            if (!raw) return -2;
            if (!raw.startsWith("/") && a[0] !== -100) {
                return (streams[a[0]] || openFiles.has(a[0])) ? -20 : -9;
            }
            const path = raw.replace(/^\/+/, "");
            if (!/^(?:out\.txt|(?:tmp\/)?tmpfile_[^/]+)$/.test(path)) return -13;
            const name = path.startsWith("tmp/") ? path.slice(4) : path;
            const result = caller.call("path_stat", {path: `${TASKNS}/${tid}/w/${name}`, nofollow: !!(flags & 0x100)});
            if (!result || result.ok !== true) return -(result?.errno || 5);
            const stat = result.stat;
            if (!stat || !Number.isInteger(stat.Mode) || typeof stat.IsDir !== "boolean") return -5;
            if (stat.IsDir || (stat.Mode & 0xf000) !== 0x8000) return -38;
            if (typeof stat.Size !== "number" || !Number.isFinite(stat.Size) || stat.Size < 0 || !Number.isInteger(stat.Size)) return -5;
            // JSON RPC supplies numbers, so larger sizes cannot be trusted losslessly.
            if (!Number.isSafeInteger(stat.Size)) return -75;
            uu.fill(0, buf, buf + 256);
            const vv = v();
            vv.setUint32(buf, 0x201, true);
            vv.setUint16(buf + 28, stat.Mode & 0xf000, true);
            vv.setBigUint64(buf + 40, BigInt(stat.Size), true);
            return 0;
        }
        case 35: { // asm-generic unlinkat; narrowly admitted task-local regular files
            if (a[2] & ~0x200) return -22; // EINVAL: unknown flags
            if (a[2] === 0x200) return -38; // AT_REMOVEDIR is not implemented
            const uu = u(), start = a[1] >>> 0;
            if (start >= uu.length) return -14;
            let end = start;
            while (end < uu.length && uu[end] !== 0) end++;
            if (end === uu.length) return -14;
            const raw = new TextDecoder().decode(uu.subarray(start, end));
            if (!raw) return -2;
            if (!raw.startsWith("/") && a[0] !== -100) return -9;
            const p = raw.replace(/^\/+/, "");
            if (p.split("/").some(seg => seg === ".." || seg === "." || seg === "")) return -13;
            const scratch = p.startsWith("tmp/tmpfile_") || p.startsWith("tmpfile_");
            if (p !== "out.txt" && !scratch) return -13;
            const target = `${TASKNS}/${tid}/w/${scratch ? p.split("/").pop() : p}`;
            const result = caller.call("path_remove", { path: target });
            return result?.ok === true ? 0 : -(result?.errno || 5);
        }
        case 29: return -25;        // ioctl: ENOTTY
        case 96: return 4242;       // set_tid_address
        case 214: return -38; // brk: no loader-owned break base; explicitly unsupported
        case 222: { // Bounded diagnostic RW arena, not Linux virtual memory.
            const len = a[1] >>> 0, flags = a[3] >>> 0;
            if (!len || (a[5] & 4095)) return -22;
            const type = flags & 3;
            if (!type) return -22;
            // Nonfixed addresses are ignored hints. fd=-1 and offset=0 are
            // shim restrictions; ENOSYS here is not native Linux policy.
            if (a[2] !== 3 || flags !== 0x22 || a[4] !== -1 || a[5] !== 0) return -38;
            const size = Math.ceil(len / 4096) * 4096;
            if (size >= 2 ** 32 || arenaNext > memoryBytes || size > memoryBytes - arenaNext) return -12;
            const end = arenaNext + size, m = inst.exports.memory;
            const pages = Math.max(0, Math.ceil((end - m.buffer.byteLength) / 65536));
            if (pages) {
                try { m.grow(pages); }
                catch (err) { if (err instanceof RangeError) return -12; throw err; }
            }
            const base = arenaNext;
            new Uint8Array(m.buffer).fill(0, base, end);
            arenaNext = end;
            return base;
        }
        case 215: { // No inaccessible holes can be enforced in linear memory.
            const start = a[0] >>> 0, len = a[1] >>> 0;
            if ((start & 4095) || !len) return -22;
            const size = Math.ceil(len / 4096) * 4096;
            if (size > 2 ** 32 - start) return -22;
            return -38;
        }
        case 226: { // Basic checks only; even zero-length requests are unsupported.
            const start = a[0] >>> 0, len = a[1] >>> 0;
            if ((start & 4095) || (a[2] & ~7)) return -22;
            const size = Math.ceil(len / 4096) * 4096;
            if (size > 2 ** 32 - start) return -12;
            return -38;
        }
        case 93: case 94: exitCode = a[0] & 255; throw { kind: "exit" };
        default: return -38;
        }
    }

    const mod = await WebAssembly.compile(wasm);
    inst = await WebAssembly.instantiate(mod, { "ferrule.linux.syscall": { syscall } });
    const m = inst.exports.memory;
    const basePages = m.buffer.byteLength / 65536;
    m.grow(2);
    arenaNext = m.buffer.byteLength;
    const stackPtr = buildInitialStack(m, basePages * 65536, 2 * 65536, argv, envp);
    try {
        inst.exports._start(stackPtr);
    } catch (err) {
        if (!(err && err.kind === "exit")) throw err;
    }
    for (const fd of [1, 2]) { try { streams[fd].flush(); } catch (_) {} }
    for (const h of openFiles.values()) { try { h.close(); } catch (_) {} }
    caller.call("exit", { code: exitCode });
}
