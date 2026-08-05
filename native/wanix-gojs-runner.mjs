// Node host for Wanix's existing GoJS worker. It intentionally forwards only
// opaque duplex/mux bytes: api.Responder remains the authority for task and
// namespace operations.
import net from "node:net";
import { MessageChannel } from "node:worker_threads";

function option(name) {
  const index = process.argv.indexOf(name);
  return index === -1 ? undefined : process.argv[index + 1];
}

const api = option("--api");
const tid = option("--tid");
if (!api || !tid) throw new Error("usage: wanix-gojs-runner.mjs --api <unix://...|tcp://...> --tid <id>");

const address = api.startsWith("unix://") ? api.slice(7) : api;
const socket = api.startsWith("tcp://")
  ? net.createConnection(new URL(api).port, new URL(api).hostname)
  : net.createConnection(address);
const { port1, port2 } = new MessageChannel();

port2.on("message", (frame) => socket.write(Buffer.from(frame)));
socket.on("data", (frame) => port2.postMessage(new Uint8Array(frame)));
socket.on("error", (error) => { throw error; });
socket.on("close", () => port2.close());

let onmessage;
globalThis.self = { addEventListener(type, listener) { if (type === "message") onmessage = listener; } };
await import(new URL("../gojs/worker/worker.js", import.meta.url));
if (!onmessage) throw new Error("Wanix GoJS worker did not register a message handler");
onmessage({ data: { worker: { tid, port: port1 } } });
