//go:build !linux && !darwin && !windows

package cli

import "errors"

func machineRAMMiB() (uint64, error) {
	return 0, errors.New("machine enrollment requires Linux, macOS or Windows")
}

func machineCPUCount() int { return 1 }
