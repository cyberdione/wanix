import assert from "node:assert/strict";
import { OpenFile, File } from "../../wasi/worker/lib.js";

let opened = 0;
let closed = 0;
const handle = {
  open() { opened++; },
  close() { closed++; },
  flush() {},
  read() { return 0; },
  write() { return 0; },
  getSize() { return 0; },
  truncate() {},
};
const file = new File(handle);
const openFile = new OpenFile(file);
assert.equal(opened, 1);
assert.equal(closed, 0);
assert.equal(openFile.fd_close(), 0);
assert.equal(closed, 1);
