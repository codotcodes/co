package cli

import (
	"os"
	"testing"
)

func TestRunnerNestedCgroupResourceLimits(t *testing.T) {
	for _, fixture := range []struct {
		name, mounts, membership string
		files                    map[string]string
		cpus, memory             uint64
	}{
		{"v2-ancestors", "1 0 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw", "0::/slice/job", map[string]string{"/sys/fs/cgroup/slice/job/cpu.max": "max 100000", "/sys/fs/cgroup/slice/cpu.max": "150000 100000", "/sys/fs/cgroup/cpu.max": "100000 100000", "/sys/fs/cgroup/slice/job/memory.max": "max", "/sys/fs/cgroup/slice/memory.max": "536870912", "/sys/fs/cgroup/memory.max": "1073741824"}, 1, 536870912},
		{"v1-ancestors", "1 0 0:1 / /cg/cpu rw - cgroup cgroup rw,cpu,cpuacct\n2 0 0:2 / /cg/mem rw - cgroup cgroup rw,memory", "2:cpu,cpuacct:/parent/job\n3:memory:/parent/job", map[string]string{"/cg/cpu/parent/job/cpu.cfs_quota_us": "-1", "/cg/cpu/parent/job/cpu.cfs_period_us": "100000", "/cg/cpu/parent/cpu.cfs_quota_us": "250000", "/cg/cpu/parent/cpu.cfs_period_us": "100000", "/cg/mem/parent/job/memory.limit_in_bytes": "1073741824", "/cg/mem/parent/memory.limit_in_bytes": "536870912"}, 3, 536870912},
		{"namespace-root", "1 0 0:1 /host/container /cg rw - cgroup2 cgroup rw", "0::/", map[string]string{"/cg/cpu.max": "50000 100000", "/cg/memory.max": "268435456"}, 1, 268435456},
		{"mount-relative", "1 0 0:1 /host/container /cg rw - cgroup2 cgroup rw", "0::/host/container/job", map[string]string{"/cg/job/cpu.max": "350000 100000"}, 4, 0},
		{"unbounded", "1 0 0:1 / /cg rw - cgroup2 cgroup rw", "0::/", map[string]string{"/cg/cpu.max": "max 100000", "/cg/memory.max": "max"}, 0, 0},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			read := func(path string) ([]byte, error) {
				if value, exists := fixture.files[path]; exists {
					return []byte(value), nil
				}
				return nil, os.ErrNotExist
			}
			cpus, memory := parseRunnerCgroupLimits(fixture.mounts, fixture.membership, read)
			if cpus != fixture.cpus || memory != fixture.memory {
				t.Fatal("cgroup budget differs", cpus, memory)
			}
		})
	}
}
