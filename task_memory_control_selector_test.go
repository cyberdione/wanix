package wanix

import "testing"

func TestMemoryControlSelectors(t *testing.T) {
	r := scratchRoot(t)
	for _, s := range memoryControlScenarios {
		c := routedTask(t, r, "linux-memory-controls-"+s)
		if c.kind != "linux" || c.MemoryControlScenario() != s || c.DiagnosticProfile() != "" {
			t.Fatal("selection", s)
		}
		child, err := r.fsys.Alloc("linux", c)
		if err != nil || child.MemoryControlScenario() != "" {
			t.Fatal("inherited", err)
		}
	}
	before := len(r.Tasks())
	id := r.fsys.nextID
	if _, err := r.fsys.AllocWithOptions("linux", r, TaskAllocOptions{Scratch: LinuxRegularScratch, DiagnosticProfile: MemoryControlProfile}); err == nil {
		t.Fatal("public control policy")
	}
	if len(r.Tasks()) != before || r.fsys.nextID != id {
		t.Fatal("allocated before rejection")
	}
	if memoryControlSelector("linux-memory-controls-rw-extra") != "" {
		t.Fatal("prefix accepted")
	}
}
