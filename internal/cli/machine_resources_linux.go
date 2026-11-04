package cli

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func machineRAMMiB() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("unable to detect RAM: %s", processError(err))
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(strings.TrimPrefix(line, "MemTotal:"))
			if len(fields) == 0 {
				break
			}
			kib, err := strconv.ParseUint(fields[0], 10, 64)
			if err != nil {
				return 0, errors.New("invalid total RAM")
			}
			return kib/1024 + min(kib%1024, 1), nil
		}
	}
	return 0, errors.New("unable to detect total RAM")
}

func machineCPUCount() int {
	cpus := runtime.NumCPU()
	var affinity unix.CPUSet
	if err := unix.SchedGetaffinity(0, &affinity); err == nil && affinity.Count() > 0 {
		cpus = affinity.Count()
	}
	cpuLimit, _ := runnerCgroupLimits()
	if cpuLimit > 0 && cpuLimit < uint64(cpus) {
		cpus = int(cpuLimit)
	}
	return max(1, cpus)
}
