package syscalls

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// workerJS is the JS source of truth this table is extracted from.
const workerJS = "../worker/worker.js"

// jsCaseNumbers extracts the numeric `case N:` labels from the raw syscall
// switch in worker.js via a deterministic plain-text scan (no regexp).
// It handles fall-through runs such as `case 226: case 215:` and ignores
// non-numeric labels like `case "path_open":`.
func jsCaseNumbers(t *testing.T, src string) map[uint32]bool {
	t.Helper()

	start := strings.Index(src, "switch (nr)")
	if start < 0 {
		t.Fatal("worker.js: syscall switch (switch (nr)) not found")
	}
	body := src[start:]
	end := strings.Index(body, "default:")
	if end < 0 {
		t.Fatal("worker.js: default clause of syscall switch not found")
	}
	body = body[:end]

	cases := make(map[uint32]bool)
	pos := 0
	for {
		i := strings.Index(body[pos:], "case ")
		if i < 0 {
			break
		}
		pos += i + len("case ")
		rest := body[pos:]
		j := 0
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if j > 0 && j < len(rest) && rest[j] == ':' {
			cases[uint32(mustAtoi(t, rest[:j]))] = true
			pos += j + 1
		} else {
			pos++ // non-numeric label; skip this occurrence
		}
	}
	if len(cases) == 0 {
		t.Fatal("worker.js: no numeric case labels found in syscall switch")
	}
	return cases
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

func loadWorkerJS(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(workerJS)
	if err != nil {
		t.Fatalf("read %s: %v (run tests from the linux/syscalls package)", workerJS, err)
	}
	return string(src)
}

// TestJSSwitchMatchesTable is the source-level cross-check: the set of
// numbers handled by the worker.js switch must equal exactly the set of
// table entries not marked unimplemented.
func TestJSSwitchMatchesTable(t *testing.T) {
	js := jsCaseNumbers(t, loadWorkerJS(t))

	var handledNrs, missingInJS, unimplInJS, inJSNotTable []uint32
	for nr := range Table {
		if handled(Table[nr]) {
			handledNrs = append(handledNrs, nr)
			if !js[nr] {
				missingInJS = append(missingInJS, nr)
			}
		} else if js[nr] {
			unimplInJS = append(unimplInJS, nr)
		}
	}
	for nr := range js {
		if _, ok := Table[nr]; !ok {
			inJSNotTable = append(inJSNotTable, nr)
		}
	}
	for _, s := range [][]uint32{handledNrs, missingInJS, unimplInJS, inJSNotTable} {
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	}

	if len(inJSNotTable) > 0 {
		t.Errorf("worker.js handles numbers missing from the table: %v", inJSNotTable)
	}
	if len(missingInJS) > 0 {
		t.Errorf("table claims handling (status != unimplemented) but worker.js switch lacks the numbers: %v", missingInJS)
	}
	if len(unimplInJS) > 0 {
		t.Errorf("entries marked unimplemented appear in the worker.js switch: %v", unimplInJS)
	}
	if len(js) != len(handledNrs) {
		t.Errorf("set mismatch: worker.js switch has %d numbers, table handles %d", len(js), len(handledNrs))
	}
}

// TestDefaultIsENOSYS pins the personality default: unknown numbers return
// -38 (ENOSYS) from the switch default clause.
func TestDefaultIsENOSYS(t *testing.T) {
	src := loadWorkerJS(t)
	for pos := 0; pos >= 0 && pos < len(src); {
		i := strings.Index(src[pos:], "default:")
		if i < 0 {
			break
		}
		abs := pos + i
		tail := src[abs:min(abs+64, len(src))]
		if strings.Contains(tail, "-38") {
			return
		}
		pos = abs + len("default:")
	}
	t.Errorf("no `default:` clause returning -38 (ENOSYS) found in worker.js")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestTableInvariants checks the table is internally consistent: valid
// statuses, key/Nr agreement, non-empty unique names.
func TestTableInvariants(t *testing.T) {
	seen := make(map[string]uint32)
	for nr, e := range Table {
		if e.Nr != nr {
			t.Errorf("table[%d].Nr = %d", nr, e.Nr)
		}
		if !validStatus(e.Status) {
			t.Errorf("table[%d] (%s): unknown status %q", nr, e.Name, e.Status)
		}
		if e.Name == "" {
			t.Errorf("table[%d]: empty name", nr)
		}
		if prev, dup := seen[e.Name]; dup {
			t.Errorf("duplicate name %q at %d and %d", e.Name, prev, nr)
		}
		seen[e.Name] = nr
	}
}

// TestHelpers checks Names, ByNumber, and Implemented behavior.
func TestHelpers(t *testing.T) {
	names := Names()
	if len(names) != len(Table) {
		t.Fatalf("Names() has %d entries, table has %d", len(names), len(Table))
	}
	nrs := sortedNrs()
	for i, nr := range nrs {
		if Table[nr].Name != names[i] {
			t.Errorf("Names()[%d] = %q, want %q (number %d)", i, names[i], Table[nr].Name, nr)
		}
	}

	if e, ok := ByNumber(56); !ok || e.Name != "openat" || e.Status != StatusRestricted {
		t.Errorf("ByNumber(56) = %+v, ok=%v", e, ok)
	}
	if _, ok := ByNumber(0); ok {
		t.Error("ByNumber(0) should miss")
	}

	impl := Implemented()
	var want []uint32
	for _, nr := range nrs {
		if handled(Table[nr]) {
			want = append(want, nr)
		}
	}
	if len(impl) != len(want) {
		t.Fatalf("Implemented() has %d entries, want %d", len(impl), len(want))
	}
	for i := range want {
		if impl[i] != want[i] {
			t.Errorf("Implemented()[%d] = %d, want %d", i, impl[i], want[i])
		}
	}
}
