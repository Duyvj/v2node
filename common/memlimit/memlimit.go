package memlimit

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

const percentOfMachine = 75

func Apply() int64 {
	if os.Getenv("GOMEMLIMIT") != "" {
		return 0
	}
	total := available(read("/proc/meminfo"), read("/sys/fs/cgroup/memory.max"), read("/sys/fs/cgroup/memory/memory.limit_in_bytes"))
	if total <= 0 {
		return 0
	}
	limit := total / 100 * percentOfMachine
	debug.SetMemoryLimit(limit)
	return limit
}

func read(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func available(meminfo, cgroupV2, cgroupV1 string) int64 {
	total := memTotal(meminfo)
	for _, raw := range []string{cgroupV2, cgroupV1} {
		v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err == nil && v > 0 && (total <= 0 || v < total) {
			total = v
		}
	}
	return total
}

func memTotal(meminfo string) int64 {
	for line := range strings.SplitSeq(meminfo, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "MemTotal:" {
			if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				return kb * 1024
			}
		}
	}
	return 0
}
