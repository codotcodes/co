package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func lookupBun() (string, error) {
	path, err := exec.LookPath("bun")
	if errors.Is(err, exec.ErrDot) {
		return path, nil
	}
	return path, err
}

func processError(err error) string {
	if errors.Is(err, exec.ErrNotFound) {
		err = syscall.ERROR_FILE_NOT_FOUND
	}
	var code syscall.Errno
	if errors.As(err, &code) {
		return fmt.Sprintf("%s (os error %d)", code.Error(), code)
	}
	return err.Error()
}

func processStatus(state *os.ProcessState) string {
	return fmt.Sprintf("exit code: %d", state.ExitCode())
}
