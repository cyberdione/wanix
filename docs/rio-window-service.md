# rio window service (fs/wsys)

The rio(4) per-window file-service subset as a kernel device (`fs/wsys`,
registered `#wsys`), plus the linux-task plumbing: the driver binds the
device at `mnt/wsys` (the `#i` → dev pattern) and the personality openat
allowlist admits exactly the admitted files. This document covers the
service as merged in N10 and the window-management slice of ADR 0007 (fleet
DAG node K1): the wctl slot verbs, the `srv/state` enumeration, and the
task-side WM grant.

Device contract tag: `wsys/0.2 (rio-4e subset)`. Wire-visible changes bump
the tag; adapters must carry the same tag.

## Task-facing files

Per-window files (rio names, 4th-edition byte layouts from the pinned
0intro/plan9 @ `0aab9506` sources: xfid.c, wctl.c, wind.c, devmouse.c), at
`mnt/wsys/<id>/`:

- `cons` — blocking console reads / console writes, multi-open (the desktop
  WM binds it onto the task stdio, rio's model)
- `consctl` — `rawon|rawoff|holdon|holdoff`, reset on close
- `mouse` — blocking single-reader 49-byte records
  `"%c%11d %11d %11d %11d "` (x y buttons msec); `'r'` on pending resize
  with rio's coalescing (multiple resizes → one `'r'`; a pending resize
  unblocks a read with no queued event; an event under a pending resize is
  rebranded `'r'`)
- `label` — verbatim read/write
- `wctl` — write: one command per write; read: single-reader blocking
  geometry line `"%11d %11d %11d %11d current|notcurrent visible|hidden "`
  armed at open (rio's wctlready), needs ≥ 4*12 bytes (`Etooshort` under
  that)

## wctl verbs (all real as of 0.2)

Grammar: rio wctl.c with N10's tokenization — whitespace fields, exact
parameter match (rio's `word()` matches prefixes; this device does not).
Rect arithmetic is rio's exactly: `-r x1 y1 x2 y2` and
`-minx/-miny/-maxx/-maxy/-dx/-dy` resolved through rio's `set(sign, neg,
abs, pos)` against the writing window's current rect — even when `-id`
retargets afterwards (rio's parsewctl ordering). Error strings are rio's,
prefixed `wsys:`: unrecognized wctl command, missing or bad wctl parameter,
extraneous text in wctl message, bad rectangle in wctl request, no such
window id, window deleted, window already hidden, window not hidden, window
allocation failed in wctl request.

- `current|top|bottom` — no parameters; the resolved line is queued on the
  writing window's srv/wctl
- `resize|move` — `-r/-minx/...`; with `-id n` the resolved line queues on
  the target's srv/wctl and `move` re-bases on the target's size (rio
  `w->screenr`). The device never resizes a slot itself: the display server
  commits geometry via srv/geom
- `new [ params... [ cmd... ] ]` — allocates a slot. Default rect is rio's
  `newrect()` cascade (600x400 at 32+16*i, cycling ten positions). Params:
  `-pid n` records the attached task id, `-hide` creates the slot hidden,
  `-scroll/-noscroll` set the scrolling state, rect params place the slot;
  `-cd` is parsed and ignored. Trailing text is the requested command,
  recorded (whitespace-normalized), never exec'd — running it in the slot
  is the display server's launch wiring (ADR 0007). Slot exhaustion is
  rio's `window allocation failed in wctl request`
- `delete [-id n]` — frees the slot (the srv/ctl close teardown:
  queues drain then EOF, mouse/wctl reads fail closed, the id is released;
  ids are never reused). Unknown ids: `no such window id`; a concurrently
  closed target: `window deleted`; any teardown failure: `wsys: I/O error
  on window close` (the signal queue layer does not produce one today; the
  mapping is the documented channel)
- `hide|unhide [-id n]` — visibility state; hides of a hidden slot fail
  `window already hidden`, unhides of a visible slot `window not hidden`.
  The wctl geometry line's last word flips visible/hidden and blocked wctl
  readers wake on the change
- `scroll|noscroll [-id n]` — scrolling state (on for new windows, rio's
  default)
- `set [-id n] [-pid n]` — records the pid on the slot; `pid <= 0` is
  ignored and still succeeds (rio wsetpid)

Host-side allocation (`#wsys/new`) is unchanged: the raw read allocates a
slot with defaults and returns the id; saturation is `signal.ErrSaturated`
there (the rio Ewalloc string is the wctl-new surface).

## Host service plane `#wsys/<id>/srv/`

`consin`, `consout`, `mouse`, `geom`, `wctl`, `notes`, `state`, `ctl` — as
merged in N10: `srv/consin` publishes console input bytes, `srv/mouse`
takes `"buttons x y"` and queues one byte-exact record (a full queue
rejects the write — busy, never drop), `srv/geom` reads/commits geometry
(flags the coalesced resize, wakes wctl readers), `srv/wctl` yields one
resolved command line per read in order, `srv/notes` yields state-change
notes (`label <label>`, `rawon|rawoff`, `holdon|holdoff`, and since 0.2:
`hide`, `unhide`, `scroll`, `noscroll`, `pid <n>`), `srv/ctl` accepts
`close` and `current on|off`.

`srv/state` (per window) is a one-line JSON status; since 0.2 it also
carries `visible`, `pid`, `scrolling` and `cmd`.

## Window enumeration `#wsys/srv/state`

The device root carries `srv/state`: one stable JSON snapshot per open,
live slots in ascending id order, ids never reused:

```json
{"version":"wsys/0.2 (rio-4e subset)","windows":[
  {"id":1,"label":"<unnamed>","rect":[32,32,632,432],"visible":true,
   "pid":0,"current":false,"scrolling":true}]}
```

Required fields per slot: `id`, `label`, `rect`, `visible`, `pid`; plus
`current`, `scrolling`, `cmd` (the recorded `wctl new` command text, empty
when absent). One open one snapshot: changes between the reads of a single
handle do not tear the listing; the next open observes them.

## Task-side grants

Freeze discipline: bind + openat allowlist only, no new syscalls.

- Object bind (the runtime registration): the raw device, srv openable —
  the display server's view, unchanged since N10.
- Default task bind — the driver binds `#wsys` at `mnt/wsys`: the device
  serves window files only. The device itself denies srv (`fs.ErrPermission`
  on stat/open; the namespace folds denied opens into not-exist) and the
  worker allowlist admits exactly `mnt/wsys/<id>/{cons,consctl,mouse,label,wctl}`.
- WM task bind — the driver binds `#wsys/wm` at `mnt/wsys` iff the task's
  launch env carries `WSYS_WM=1`: the device grant adds the srv plane (the
  enumeration and every window's srv files) and the worker allowlist admits
  `mnt/wsys/srv/state` and `mnt/wsys/<id>/srv/*`. The grant is fixed at
  task creation; no task can set it for itself through the personality
  (path_open only admits the pre-authorized paths), and it widens nothing
  outside `mnt/wsys`.
- Single-window binds (`#wsys/<id>`) keep N10 semantics: the device's Route
  hands them straight to the raw window, so their srv visibility remains
  gated by the worker allowlist (the same window object serves the display
  server through the root).

## Device ↔ host slot handshake

Slots are host-served surfaces: the device records intent and state; the
display server acts through the srv plane.

1. **Create.** The WM writes `new [-pid n] [-hide] [rect params] [cmd]` to
   its own wctl (or the host reads `#wsys/new` for a bare slot). The device
   allocates the id (never reused), records rect/pid/hidden/scrolling/cmd,
   and queues `new <id> <x1> <y1> <x2> <y2>` on the new slot's srv/wctl.
2. **Discover.** The host enumerates `srv/state` (or the device root
   listing) and finds the slot; opening its srv/wctl drains the `new`
   announcement, opening srv/geom reads the recorded rect.
3. **Attach.** The launch wiring (display server duty, ADR 0007) starts the
   task and the WM attaches it: `wctl new -pid n` at creation, or
   `set -id <slot> -pid <taskid>` after. `srv/state` reflects `pid`; the
   host wires the task's stdio onto the slot's cons files as in N10.
4. **Serve.** Geometry commits arrive as srv/geom writes (waking the
   slot's wctl reader, arming the coalesced mouse resize); console input is
   published via srv/consin; mouse events via srv/mouse (byte-exact 49-byte
   records, coalescing, full-queue busy). The slot's task-side files keep
   N10 semantics exactly: blocking reads, single-reader mouse/wctl slots,
   consctl close-reset.
5. **Manage.** hide/unhide/scroll/noscroll/set/top/bottom/current/resize/
   move from any WM-granted wctl; every state change is noted on the slot's
   srv/notes and reflected in srv/state and the wctl geometry line.
6. **Teardown.** `delete` (or srv/ctl `close`) drains the queues, fails
   reads closed, releases the id; the enumeration drops the slot on the
   next open. The attached task is not signalled by the device — stopping
   it is the host's policy.

## Explicit limitations of this slice

- The device does not exec: `wctl new` records the requested command text;
  launch wiring is the display server's.
- Geometry is trusted from srv/geom writes; the WM applies its own clamps.
- No buttonmap/cursor/snarf/winname/text/image access; cons has no line
  editing or hold queueing (state only).
- rio's `action disallowed when mouse active` (button-held gating for
  top-or-worse verbs) is not modeled: the device does not track global
  button state.
- Z-order (top/bottom) and focus (current) are display-server duties; the
  device queues the intent and reports `current` only as recorded via
  srv/ctl.
