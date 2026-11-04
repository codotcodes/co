package cli

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
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
	if data, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) == 2 {
			quota, qerr := strconv.ParseUint(fields[0], 10, 64)
			period, perr := strconv.ParseUint(fields[1], 10, 64)
			if qerr == nil && perr == nil && period > 0 {
				bounded := quota/period + min(quota%period, 1)
				if bounded < uint64(cpus) {
					cpus = int(max(1, bounded))
				}
			}
		}
	}
	return cpus
}
