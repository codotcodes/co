package cli

import (
	"context"
	"io"
	"os"

	"golang.org/x/sys/windows"
)

func openSecretTerminal() (*os.File, io.Writer, int, func(), error) {
	input, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, 0, nil, err
	}
	output, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		input.Close()
		return nil, nil, 0, nil, err
	}
	return input, output, int(input.Fd()), func() { _ = input.Close(); _ = output.Close() }, nil
}

func interruptSecretTerminal(file *os.File) {
	_ = windows.CancelIoEx(windows.Handle(file.Fd()), nil)
	_ = file.Close()
}

func readSecretFileContext(ctx context.Context, file *os.File, limit int) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(file, int64(limit+1)))
		done <- result{data, err}
	}()
	select {
	case read := <-done:
		return read.data, read.err
	case <-ctx.Done():
		interruptSecretTerminal(file)
		return nil, ctx.Err()
	}
}
