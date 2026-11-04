package cli

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

func machineRAMMiB() (uint64, error) {
	bytes, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("unable to detect RAM: %s", processError(err))
	}
	return bytes/(1024*1024) + min(bytes%(1024*1024), 1), nil
}

func machineCPUCount() int { return runtime.NumCPU() }
