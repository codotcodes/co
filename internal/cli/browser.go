package cli

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os/exec"
	"strings"
)

func openBrowser(ctx context.Context, target string, stdin io.Reader, stdout, stderr io.Writer) error {
	if !validBrowserURL(target) {
		return errors.New("invalid browser URL")
	}
	return platformBrowser(ctx, target, stdin, stdout, stderr)
}

func validBrowserURL(target string) bool {
	parsed, err := url.Parse(target)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" && parsed.User == nil
}

func browserCommand(ctx context.Context, args []string, foreground bool, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("empty browser command")
	}
	path, err := exec.LookPath(args[0])
	if err != nil && !errors.Is(err, exec.ErrDot) {
		return err
	}
	command := &exec.Cmd{Path: path, Args: args}
	if foreground {
		command = exec.CommandContext(ctx, path, args[1:]...)
		command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
		return command.Run()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

func asciiFields(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' })
}

func browserArguments(spec, target string) []string {
	args := asciiFields(spec)
	if len(args) == 0 {
		return nil
	}
	used := false
	for index, arg := range args {
		var out strings.Builder
		for position := 0; position < len(arg); position++ {
			if arg[position] == '%' && position+1 < len(arg) {
				switch arg[position+1] {
				case 's':
					out.WriteString(target)
					used = true
					position++
					continue
				case 'c':
					out.WriteByte(':')
					position++
					continue
				case '%':
					out.WriteByte('%')
					position++
					continue
				}
			}
			out.WriteByte(arg[position])
		}
		args[index] = out.String()
	}
	if len(args) > 0 && !used {
		args = append(args, target)
	}
	return args
}
