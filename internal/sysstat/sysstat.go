// Package sysstat reads point-in-time process and container resource stats:
// the Go runtime's own memory footprint, plus the cgroup memory limit/usage that
// Docker imposes via `mem_limit` (SYSTEM.md §9). It is deliberately tiny and
// dependency-free, and every host-level read is best-effort: on a platform
// without cgroups (e.g. macOS dev) the container fields report Available=false
// rather than erroring, so the metrics surface degrades gracefully.
package sysstat

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Process is the Go runtime's own footprint — the brainiac process, not the box.
type Process struct {
	Goroutines     int    `json:"goroutines"`
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	HeapSysBytes   uint64 `json:"heap_sys_bytes"`
	NumGC          uint32 `json:"num_gc"`
	NumCPU         int    `json:"num_cpu"`
}

// Container is the cgroup memory ceiling and current usage — the "allocated
// resources" an operator watches. Available is false when no cgroup limit is
// readable (unlimited, or a non-Linux host); the byte fields are then zero and
// UsedPercent is 0.
type Container struct {
	Available     bool    `json:"available"`
	MemLimitBytes uint64  `json:"mem_limit_bytes"`
	MemUsedBytes  uint64  `json:"mem_used_bytes"`
	UsedPercent   float64 `json:"used_percent"`
}

// ReadProcess snapshots the Go runtime's memory and scheduler stats.
func ReadProcess() Process {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return Process{
		Goroutines:     runtime.NumGoroutine(),
		HeapAllocBytes: m.HeapAlloc,
		HeapSysBytes:   m.HeapSys,
		NumGC:          m.NumGC,
		NumCPU:         runtime.NumCPU(),
	}
}

// ReadContainer reads the cgroup memory limit and current usage, trying cgroup
// v2 first (unified hierarchy) then falling back to v1. A missing file, an
// unparseable value, or a sentinel "max"/absurdly-high limit all yield
// Available=false — the caller shows "n/a", never a wrong number.
func ReadContainer() Container {
	limit, okL := readCgroupBytes(
		"/sys/fs/cgroup/memory.max",                   // v2
		"/sys/fs/cgroup/memory/memory.limit_in_bytes", // v1
	)
	used, okU := readCgroupBytes(
		"/sys/fs/cgroup/memory.current",               // v2
		"/sys/fs/cgroup/memory/memory.usage_in_bytes", // v1
	)
	// A cgroup with no limit reports "max" (v2) or a near-uint64-max sentinel
	// (v1). Treat those as "no ceiling to watch" rather than a real number.
	if !okL || !okU || limit == 0 || limit >= 1<<62 {
		return Container{}
	}
	c := Container{Available: true, MemLimitBytes: limit, MemUsedBytes: used}
	c.UsedPercent = 100 * float64(used) / float64(limit)
	return c
}

// readCgroupBytes returns the first path that holds a parseable byte count. The
// v2 "max" sentinel (no limit) is reported as not-ok so callers skip it.
func readCgroupBytes(paths ...string) (uint64, bool) {
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s == "max" {
			return 0, false
		}
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			continue
		}
		return n, true
	}
	return 0, false
}
