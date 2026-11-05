# Bounded task input queue (experimental)

`#wminput/new` allocates a queue and returns a random 128-bit hexadecimal ID. The allocator caps live endpoints at 64 by default (configurable).
The host binds `#wminput/<id>/read` into exactly one task namespace and writes
input frames through `#wminput/<id>/write`. The reader blocks when empty. A
read can split frames; applications must use an explicit framing convention
(such as UTF-8 JSON Lines) and retain partial records. This payload is not the
Wanix duplex-CBOR RPC envelope and is not a 9P protocol extension.

Writes are atomic and limited to 32 KiB queued per endpoint. A write that does
not fit returns `signal.ErrSaturated` and enqueues nothing; the producer must
surface busy or retry after a later consumer read. No input is silently dropped.
One reader is permitted per endpoint. Reader calls use the task context, so
exit/cancellation interrupts a blocked read. Context cancellation closes and
reclaims the endpoint. The owner must close its read handle or endpoint `ctl` on task teardown; a bounded live-endpoint cap prevents unbounded allocator growth. `ctl` accepts `close`, closes the queue, drains already
queued bytes, then yields EOF; new opens fail after close. This API does not
implement WM surface ownership, resize, final-render ordering, DOM updates or
rio `/dev/draw`.

The device is exposed at `#wminput` in the Wasm runtime. Host integration must
allocate an ID, bind its read endpoint into the task namespace and write event
frames to its write endpoint. Do not use this endpoint through the 9P adapter
until the particular exported namespace and fid-close semantics are tested.
