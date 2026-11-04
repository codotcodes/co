//go:build !windows

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openSecretTerminal() (*os.File, io.Writer, int, func(), error) {
	file, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, 0, nil, err
	}
	connection, err := file.SyscallConn()
	if err != nil {
		file.Close()
		return nil, nil, 0, nil, err
	}
	var descriptor int
	if err := connection.Control(func(fd uintptr) { descriptor = int(fd) }); err != nil {
		file.Close()
		return nil, nil, 0, nil, err
	}
	return file, file, descriptor, func() { _ = file.Close() }, nil
}

func interruptSecretTerminal(file *os.File) {
	_ = file.Close()
}

func readSecretFileContext(ctx context.Context, file *os.File, limit int) ([]byte, error) {
	descriptor, err := secretFileDescriptor(file)
	if err != nil {
		return nil, err
	}
	var data []byte
	buffer := make([]byte, min(4096, limit+1))
	for len(data) <= limit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fds := []unix.PollFd{{Fd: int32(descriptor), Events: unix.POLLIN}}
		ready, err := unix.Poll(fds, 50)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if ready == 0 {
			continue
		}
		n, err := unix.Read(descriptor, buffer[:min(len(buffer), limit+1-len(data))])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return data, nil
		}
		data = append(data, buffer[:n]...)
	}
	return data, nil
}
