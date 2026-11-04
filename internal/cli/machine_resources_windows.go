package cli

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func machineRAMMiB() (uint64, error) {
	var memory struct {
		Length, Load                                                                                                uint32
		TotalPhysical, AvailablePhysical, TotalPage, AvailablePage, TotalVirtual, AvailableVirtual, ExtendedVirtual uint64
	}
	memory.Length = uint32(unsafe.Sizeof(memory))
	status := windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
	ok, _, err := status.Call(uintptr(unsafe.Pointer(&memory)))
	if ok == 0 {
		return 0, fmt.Errorf("unable to detect RAM: %s", processError(err))
	}
	return memory.TotalPhysical/(1024*1024) + min(memory.TotalPhysical%(1024*1024), 1), nil
}

func machineCPUCount() int { return runtime.NumCPU() }
