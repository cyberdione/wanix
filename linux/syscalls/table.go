// Package syscalls is the canonical Go record of the Linux-personality
// syscall surface that linux/worker/worker.js serves to wasm32 guests
// importing "ferrule.linux.syscall".
//
// The worker exports a single syscall(nr, a0..a5) import. Syscall numbers
// follow the asm-generic 64-bit (aarch64/riscv64) unistd.h numbering, and
// error returns are negative Linux errno values (-38 is ENOSYS). Numbers
// absent from the switch fall through to a default case returning -38.
// Guest memory is capped at 16 MiB, and the guest enters through _start
// with a Linux-style initial stack (argc/argv/envp plus AT_PAGESZ and
// AT_NULL auxv entries).
//
// Entry status values:
//
//	implemented    real, Linux-conformant behavior for this personality
//	restricted     real behavior, but capability-narrowed (paths, mapping
//	               kinds); outside the subset the call fails with an errno
//	noop           returns success without performing the operation
//	enosys-noted   handled explicitly in the switch as a refusal stub
//	               that answers with a specific errno (here ENOTTY) rather
//	               than the default -38 fall-through
//	unimplemented  absent from the switch; falls through to default -38
//
// This table is extracted, not authoritative: table_test.go cross-checks
// it against the worker.js switch on every test run. It is the seed for
// moving the personality semantics out of JS and into Go.
package syscalls

import "sort"

// Status values for Entry.Status.
const (
	StatusImplemented   = "implemented"
	StatusRestricted    = "restricted"
	StatusNoop          = "noop"
	StatusEnosysNoted   = "enosys-noted"
	StatusUnimplemented = "unimplemented"
)

// Entry describes one syscall number in the personality surface.
type Entry struct {
	Nr     uint32 // asm-generic 64-bit syscall number; matches the Table key
	Name   string // asm-generic name (aliases noted in Notes)
	Status string // one of the Status* constants
	Notes  string
}

// Table is the canonical syscall surface, keyed by asm-generic syscall
// number. Entries marked unimplemented are known-absent numbers (they hit
// the default -38 case) and are recorded to make the deliberate gaps
// visible.
var Table = map[uint32]Entry{
	29:  {Nr: 29, Name: "ioctl", Status: StatusEnosysNoted, Notes: "Refusal stub: every request returns -25 (ENOTTY) instead of falling through to the default -38 (ENOSYS)."},
	35:  {Nr: 35, Name: "unlinkat", Status: StatusNoop, Notes: "Returns 0 without deleting anything; paired with case 87 as fake success for scratch-file removal."},
	56:  {Nr: 56, Name: "openat", Status: StatusRestricted, Notes: "dirfd must be AT_FDCWD (-100) or EBADF; after normalization (empty or '..' segments give EACCES) only out.txt and tmp/tmpfile_* scratch names are admitted; created via path_touch, served by FileHandle, fresh fd from 3."},
	57:  {Nr: 57, Name: "close", Status: StatusImplemented, Notes: "Closes fds in the open-file table only; unknown fds return EBADF (-9); std streams are not closeable."},
	62:  {Nr: 62, Name: "llseek", Status: StatusImplemented, Notes: "Open files only (EBADF otherwise); whence outside 0..2 is EINVAL; writes the new 64-bit offset little-endian to result_ptr (legacy _llseek signature)."},
	63:  {Nr: 63, Name: "read", Status: StatusImplemented, Notes: "fd 0 serves task-provided stdin text until exhausted (then 0); otherwise reads streams then open files; EBADF for unknown fds."},
	64:  {Nr: 64, Name: "write", Status: StatusImplemented, Notes: "Writes to streams first, then open files; EBADF for unknown fds."},
	65:  {Nr: 65, Name: "readv", Status: StatusImplemented, Notes: "Validates the iovec array (cnt <= 1024, in-bounds) then reads vectors sequentially through the read(63) path, stopping on a short read."},
	66:  {Nr: 66, Name: "writev", Status: StatusImplemented, Notes: "Validates the iovec array (cnt <= 1024, in-bounds), gathers the vectors, and performs one write."},
	87:  {Nr: 87, Name: "timerfd_gettime", Status: StatusNoop, Notes: "Handled with case 35 as fake success ('unlink of scratch' per the JS comment). asm-generic 87 is timerfd_gettime while x86-64 87 is unlink; the pairing suggests the author targeted the x86-64 unlink number. Resolve when porting."},
	93:  {Nr: 93, Name: "exit", Status: StatusImplemented, Notes: "Masks the code with 255 and unwinds via an internal exit sentinel; the run loop then flushes/closes descriptors and reports the task exit."},
	94:  {Nr: 94, Name: "exit_group", Status: StatusImplemented, Notes: "Same handling as exit(93)."},
	96:  {Nr: 96, Name: "set_tid_address", Status: StatusNoop, Notes: "Returns the fixed fake tid 4242; no tid-address semantics are tracked."},
	214: {Nr: 214, Name: "brk", Status: StatusNoop, Notes: "Echoes the requested break address back; no heap management."},
	215: {Nr: 215, Name: "munmap", Status: StatusNoop, Notes: "Returns 0; nothing is unmapped (memory only grows)."},
	222: {Nr: 222, Name: "mmap", Status: StatusRestricted, Notes: "Anonymous mappings only: flags must include MAP_ANONYMOUS (0x20) and fd must be -1, else ENOSYS (-38); grows linear memory by rounded pages and returns the previous end, or ENOMEM (-12) past the 16 MiB cap."},
	226: {Nr: 226, Name: "mprotect", Status: StatusNoop, Notes: "Returns 0; no permission changes."},
	291: {Nr: 291, Name: "statx", Status: StatusRestricted, Notes: "Limited to the same out.txt/scratch paths as openat; zero-fills 256 bytes then synthesizes a regular file (mask 0x7ff, blksize 4096, mode 0100644, ino 42, size from a path_size call)."},

	// Known-absent numbers: deliberately unimplemented, default -38.
	61:  {Nr: 61, Name: "getdents64", Status: StatusUnimplemented, Notes: "No directory listing; hits the default -38."},
	78:  {Nr: 78, Name: "readlinkat", Status: StatusUnimplemented, Notes: "Hits the default -38."},
	79:  {Nr: 79, Name: "newfstatat", Status: StatusUnimplemented, Notes: "statx(291) is the only stat path; hits the default -38."},
	80:  {Nr: 80, Name: "fstat", Status: StatusUnimplemented, Notes: "statx(291) is the only stat path; hits the default -38."},
	113: {Nr: 113, Name: "clock_gettime", Status: StatusUnimplemented, Notes: "Hits the default -38."},
	172: {Nr: 172, Name: "getpid", Status: StatusUnimplemented, Notes: "Hits the default -38."},
	278: {Nr: 278, Name: "getrandom", Status: StatusUnimplemented, Notes: "Hits the default -38."},
}

// validStatus reports whether s is a known status constant.
func validStatus(s string) bool {
	switch s {
	case StatusImplemented, StatusRestricted, StatusNoop, StatusEnosysNoted, StatusUnimplemented:
		return true
	}
	return false
}

// handled reports whether the worker implements the entry explicitly
// (everything except unimplemented entries appears in the JS switch).
func handled(e Entry) bool { return e.Status != StatusUnimplemented }

// sortedNrs returns all table numbers in ascending order.
func sortedNrs() []uint32 {
	nrs := make([]uint32, 0, len(Table))
	for nr := range Table {
		nrs = append(nrs, nr)
	}
	sort.Slice(nrs, func(i, j int) bool { return nrs[i] < nrs[j] })
	return nrs
}

// Names returns the entry names ordered by syscall number.
func Names() []string {
	names := make([]string, 0, len(Table))
	for _, nr := range sortedNrs() {
		names = append(names, Table[nr].Name)
	}
	return names
}

// ByNumber looks up an entry by asm-generic syscall number.
func ByNumber(nr uint32) (Entry, bool) {
	e, ok := Table[nr]
	return e, ok
}

// Implemented returns, in ascending order, the syscall numbers the worker
// handles explicitly in its switch — every status except unimplemented.
func Implemented() []uint32 {
	var nrs []uint32
	for _, nr := range sortedNrs() {
		if handled(Table[nr]) {
			nrs = append(nrs, nr)
		}
	}
	return nrs
}
