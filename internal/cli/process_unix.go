//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func lookupBun() (string, error) {
	return lookupExecutable("bun")
}

func lookupExecutable(name string) (string, error) {
	path, present := os.LookupEnv("PATH")
	if !present {
		path = "/bin:/usr/bin"
	}
	last := error(syscall.ENOENT)
	for _, directory := range strings.Split(path, ":") {
		candidate := name
		if directory != "" {
			candidate = directory + "/" + name
		}
		info, err := os.Stat(candidate)
		if err == nil {
			if info.IsDir() {
				err = syscall.EACCES
			} else {
				err = syscall.Access(candidate, 1)
			}
		}
		if err == nil {
			return candidate, nil
		}
		switch {
		case errors.Is(err, syscall.EACCES):
			last = syscall.EACCES
		case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ENOTDIR):
		default:
			return "", err
		}
	}
	return "", last
}

func processError(err error) string {
	if errors.Is(err, exec.ErrNotFound) {
		err = syscall.ENOENT
	}
	var code syscall.Errno
	if errors.As(err, &code) {
		message := code.Error()
		return fmt.Sprintf("%s%s (os error %d)", strings.ToUpper(message[:1]), message[1:], code)
	}
	return err.Error()
}

func processStatus(state *os.ProcessState) string {
	status := state.Sys().(syscall.WaitStatus)
	if !status.Signaled() {
		return fmt.Sprintf("exit status: %d", state.ExitCode())
	}
	signal := status.Signal()
	names := map[syscall.Signal]string{
		syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT",
		syscall.SIGILL: "SIGILL", syscall.SIGTRAP: "SIGTRAP", syscall.SIGABRT: "SIGABRT",
		syscall.SIGBUS: "SIGBUS", syscall.SIGFPE: "SIGFPE", syscall.SIGKILL: "SIGKILL",
		syscall.SIGUSR1: "SIGUSR1", syscall.SIGSEGV: "SIGSEGV", syscall.SIGUSR2: "SIGUSR2",
		syscall.SIGPIPE: "SIGPIPE", syscall.SIGALRM: "SIGALRM", syscall.SIGTERM: "SIGTERM",
		syscall.SIGCHLD: "SIGCHLD", syscall.SIGCONT: "SIGCONT", syscall.SIGSTOP: "SIGSTOP",
		syscall.SIGTSTP: "SIGTSTP", syscall.SIGTTIN: "SIGTTIN", syscall.SIGTTOU: "SIGTTOU",
		syscall.SIGURG: "SIGURG", syscall.SIGXCPU: "SIGXCPU", syscall.SIGXFSZ: "SIGXFSZ",
		syscall.SIGVTALRM: "SIGVTALRM", syscall.SIGPROF: "SIGPROF", syscall.SIGWINCH: "SIGWINCH",
		syscall.SIGIO: "SIGIO", syscall.SIGSYS: "SIGSYS",
	}
	result := fmt.Sprintf("signal: %d", signal)
	if name := names[signal]; name != "" {
		result += " (" + name + ")"
	}
	if status.CoreDump() {
		result += " (core dumped)"
	}
	return result
}
