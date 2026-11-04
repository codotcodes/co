package cli

import (
	"context"
	"os/exec"
)

func isolateRunnerProcess(*exec.Cmd) {}

func killRunnerProcess(command *exec.Cmd) {
	_ = command.Process.Kill()
}

func waitRunnerProcess(ctx context.Context, child *exec.Cmd) (int, bool) {
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return 0, true
		}
		if child.ProcessState != nil && child.ProcessState.ExitCode() >= 0 {
			return child.ProcessState.ExitCode(), true
		}
		return 1, true
	case <-ctx.Done():
		killRunnerProcess(child)
		<-done
		return 1, false
	}
}
