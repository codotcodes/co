package cli

import (
	"context"
	"io"

	"golang.org/x/sys/windows"
)

func platformBrowser(ctx context.Context, target string, _ io.Reader, _, _ io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	url, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, url, nil, nil, windows.SW_SHOWNORMAL)
}
