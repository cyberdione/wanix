// Privileged capture-2 launch assembly; never accepts guest loader/guard overrides.
export function createCapture2Runner({createPairedLoader, createLinuxPersonality, extractWasm, isCopyFault}) {
    return async function runCapture2(e, caller) {
        const data = structuredClone(e.data);
        const identity = data.tid;
        if (!Number.isInteger(identity) || identity <= 0 || identity > 2147483647)
            throw new TypeError('positive diagnostic task identity required');
        const random16 = new Uint8Array(16);
        globalThis.crypto.getRandomValues(random16);
        const options = {argv: data.args, envp: data.env, execfn: data.args[0], random16,
            credentials: {uid: 0, euid: 0, gid: 0, egid: 0}};
        const source = new Uint8Array(extractWasm(data.bin));
        const token = Object.freeze({});
        let code, authority, personality;
        const memory = {
            get byteLength() { return authority.byteLength; },
            read(at, size) { return authority.copyin(at, size); },
            write(at, bytes) { authority.copyout(at, bytes); },
            check(at, size, prot) { authority.check(at, size, prot); },
        };
        const cleanup = () => personality?.cleanup();
        const syscall = (syscalls, nr, a0, a1, a2, a3, a4, a5) => {
            if (authority && authority !== syscalls) throw new TypeError('changed memory authority');
            authority = syscalls;
            if (!Number.isSafeInteger(authority.byteLength)) throw new TypeError('backing extent required');
            if ([214, 215, 222, 226].includes(nr)) return syscalls.syscall(nr, a0, a1, a2, a3, a4, a5);
            personality ??= createLinuxPersonality({data}, caller, memory, {
                identity, exit(value) { code = value; throw token; },
            });
            try { return personality.syscall(nr, a0, a1, a2, a3, a4, a5); }
            catch (error) { if (isCopyFault(error)) return -14; throw error; }
        };
        const loader = createPairedLoader({
            hostOptions: () => options,
            instantiate: (bytes, imports) => WebAssembly.instantiate(bytes, imports),
            hostImports: {'ferrule.linux.syscall': {syscall}},
            cleanup, exitCode: error => error === token ? code : undefined,
        });
        const handle = await loader.load(source);
        const outcome = await handle.run();
        if (outcome.kind === 'return' || outcome.kind === 'exit')
            caller.call('exit', {code: outcome.kind === 'exit' ? outcome.code : 0});
        else {
            const error = new Error(`capture-2 terminal ${outcome.kind}`);
            error.outcome = outcome;
            throw error;
        }
        return outcome;
    };
}
