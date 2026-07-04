package sysstat

import "testing"

func TestReadProcess(t *testing.T) {
	p := ReadProcess()
	if p.NumCPU < 1 {
		t.Errorf("NumCPU = %d, want >= 1", p.NumCPU)
	}
	if p.Goroutines < 1 {
		t.Errorf("Goroutines = %d, want >= 1 (this test runs in one)", p.Goroutines)
	}
	if p.HeapSysBytes == 0 {
		t.Error("HeapSysBytes = 0, want a live heap")
	}
}

// ReadContainer must never panic and must report Available=false when no cgroup
// limit is readable (the macOS-dev / CI case). We can't assert a specific value,
// but an unavailable container must carry zeroed byte fields.
func TestReadContainerGracefulOffLinux(t *testing.T) {
	c := ReadContainer()
	if !c.Available {
		if c.MemLimitBytes != 0 || c.MemUsedBytes != 0 || c.UsedPercent != 0 {
			t.Errorf("unavailable container should be zeroed, got %+v", c)
		}
	} else if c.MemLimitBytes == 0 {
		t.Error("available container reported a zero limit")
	}
}
