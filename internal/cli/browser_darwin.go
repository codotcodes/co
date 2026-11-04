package cli

import (
	"context"
	"io"
)

func platformBrowser(ctx context.Context, target string, stdin io.Reader, stdout, stderr io.Writer) error {
	return browserCommand(ctx, []string{"/usr/bin/open", target}, true, stdin, stdout, stderr)
}
