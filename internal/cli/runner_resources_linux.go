package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func runnerCgroupLimits() (uint64, uint64) {
	mounts, _ := os.ReadFile("/proc/self/mountinfo")
	membership, _ := os.ReadFile("/proc/self/cgroup")
	cpus, memory := parseRunnerCgroupLimits(string(mounts), string(membership), os.ReadFile)
	rootCPUs, rootMemory := parseRunnerCgroupLimits("1 0 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw", "0::/", os.ReadFile)
	if rootCPUs > 0 && (cpus == 0 || rootCPUs < cpus) {
		cpus = rootCPUs
	}
	if rootMemory > 0 && (memory == 0 || rootMemory < memory) {
		memory = rootMemory
	}
	return cpus, memory
}

func parseRunnerCgroupLimits(mounts, membership string, read func(string) ([]byte, error)) (uint64, uint64) {
	cpuLimit, memoryLimit := uint64(0), uint64(0)
	bound := func(current *uint64, value uint64) {
		if value > 0 && (*current == 0 || value < *current) {
			*current = value
		}
	}
	readNumber := func(path string) (uint64, bool) {
		data, err := read(path)
		if err != nil {
			return 0, false
		}
		value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		return value, err == nil
	}
	decodeMountPath := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, mountLine := range strings.Split(mounts, "\n") {
		left, right, found := strings.Cut(mountLine, " - ")
		before, after := strings.Fields(left), strings.Fields(right)
		if !found || len(before) < 5 || len(after) < 3 || after[0] != "cgroup2" && after[0] != "cgroup" {
			continue
		}
		mountRoot, mountPoint := decodeMountPath.Replace(before[3]), decodeMountPath.Replace(before[4])
		for _, memberLine := range strings.Split(membership, "\n") {
			parts := strings.SplitN(memberLine, ":", 3)
			if len(parts) != 3 {
				continue
			}
			unified := after[0] == "cgroup2" && parts[1] == ""
			cpu, memory := unified, unified
			if after[0] == "cgroup" {
				for _, controller := range strings.Split(parts[1], ",") {
					mounted := strings.Contains(","+after[2]+",", ","+controller+",")
					cpu = cpu || controller == "cpu" && mounted
					memory = memory || controller == "memory" && mounted
				}
			}
			if !cpu && !memory {
				continue
			}
			relative := parts[2]
			if relative == mountRoot {
				relative = "/"
			} else if mountRoot != "/" && strings.HasPrefix(relative, mountRoot+"/") {
				relative = strings.TrimPrefix(relative, mountRoot)
			}
			current := filepath.Join(mountPoint, strings.TrimPrefix(filepath.Clean(relative), "/"))
			for {
				if cpu {
					quota, period := uint64(0), uint64(0)
					if unified {
						if data, err := read(filepath.Join(current, "cpu.max")); err == nil {
							fields := strings.Fields(string(data))
							if len(fields) == 2 {
								quota, _ = strconv.ParseUint(fields[0], 10, 64)
								period, _ = strconv.ParseUint(fields[1], 10, 64)
							}
						}
					} else {
						quota, _ = readNumber(filepath.Join(current, "cpu.cfs_quota_us"))
						period, _ = readNumber(filepath.Join(current, "cpu.cfs_period_us"))
					}
					if quota > 0 && period > 0 {
						bound(&cpuLimit, quota/period+min(quota%period, 1))
					}
				}
				if memory {
					name := "memory.limit_in_bytes"
					if unified {
						name = "memory.max"
					}
					if value, ok := readNumber(filepath.Join(current, name)); ok {
						// One byte retains a zero-byte cgroup limit through the
						// zero-as-unbounded representation and still yields zero MiB.
						bound(&memoryLimit, max(1, value))
					}
				}
				if current == mountPoint {
					break
				}
				parent := filepath.Dir(current)
				if parent == current || !strings.HasPrefix(parent+string(filepath.Separator), mountPoint+string(filepath.Separator)) {
					break
				}
				current = parent
			}
		}
	}
	return cpuLimit, memoryLimit
}

func runnerHostMemoryBudget() (uint64, error) {
	memory, err := machineRAMMiB()
	if err != nil {
		return 0, err
	}
	_, limit := runnerCgroupLimits()
	if limit > 0 {
		memory = min(memory, limit/(1024*1024))
	}
	return memory, nil
}
