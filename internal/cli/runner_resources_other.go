//go:build !linux

package cli

func runnerHostMemoryBudget() (uint64, error) { return machineRAMMiB() }
