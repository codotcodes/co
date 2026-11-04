//go:build !windows

package cli

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func isolateRunnerProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killRunnerProcess(command *exec.Cmd) {
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	_ = command.Process.Kill()
}

// Runner commands use only file-backed streams, so exec starts no copy workers.
// A single reaper keeps the leader PID reserved until cancellation has killed
// its process group. Concurrent Cmd.Wait would release that PID too early.
func waitRunnerProcess(ctx context.Context, child *exec.Cmd) (int, bool) {
	defer child.Process.Release()
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(child.Process.Pid, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return 1, true
		}
		if pid != 0 {
			if status.Exited() {
				return status.ExitStatus(), true
			}
			return 1, true
		}
		if ctx.Err() != nil {
			killRunnerProcess(child)
			for {
				_, err := syscall.Wait4(child.Process.Pid, &status, 0, nil)
				if !errors.Is(err, syscall.EINTR) {
					break
				}
			}
			return 1, false
		}
		_ = waitForDevice(ctx, 200*time.Millisecond)
	}
}
