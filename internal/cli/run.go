// Package cli contains the Go migration of the co command-line client.
package cli

import (
	"fmt"
	"io"
)

// Version is overridden by release builds with -ldflags -X.
var Version = "0.13.0"

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	command := "help"
	if len(args) != 0 {
		command = args[0]
	}
	switch command {
	case "login":
		return login(stdin, stdout, stderr)
	case "version", "--version", "-V":
		_, err := fmt.Fprintln(stdout, "co", Version)
		return err
	case "workflow":
		return runWorkflow(args[1:], stdin, stdout, stderr)
	case "whoami":
		return whoami(stdout)
	case "logout":
		return logout(stdout, stderr)
	case "help", "--help", "-h":
		_, err := fmt.Fprintln(stdout, "co: partial Go migration\n\n  login                   authorize this machine\n  workflow check [FILE]   validate local workflow plans\n  whoami                  show the signed-in account\n  logout                  revoke the local session\n  version                 print version\n\nOther commands still require the Rust CLI. This binary never falls back to it.")
		return err
	default:
		return fmt.Errorf("command %q is not migrated to Go; use the retained Rust CLI", command)
	}
}
