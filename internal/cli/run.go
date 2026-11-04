// Package cli contains the Go migration of the co command-line client.
package cli

import (
	"fmt"
	"io"
)

// Version is overridden by release builds with -ldflags -X.
var Version = "0.13.0"

const partialHelp = `co: partial Go migration

  login                   authorize this machine
  workflow check [FILE]   validate local workflow plans
  whoami                  show the signed-in account
  logout                  revoke the local session
  git-credential <op>      serve scoped Git credentials
  access view <repo>       read a repository using an agent grant
  access wait [request]   wait for a saved access request
  access request [opts] <repo>  request human-approved agent access
  agent register <name>   register a local agent identity
  agent list              list local agent identities
  agent attest <repo> <oid>...  attest explicitly selected commits
  repo view <repo>        show repository metadata
  repo create [opts] <repo>  create a repository
  version                 print version

Other commands still require the Rust CLI. This binary never falls back to it.`

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	command := "help"
	if len(args) != 0 {
		command = args[0]
	}
	switch command {
	case "repo":
		return repositoryCommand(args[1:], stdout, stderr)
	case "access":
		if len(args) >= 2 && args[1] == "request" {
			return requestAgentAccess(args[2:], stdin, stdout, stderr)
		}
		if len(args) >= 2 && args[1] == "wait" {
			return waitAgentAccess(args[2:], stdout, stderr)
		}
		if len(args) == 3 && args[1] == "view" {
			return agentRepositoryView(args[2], stdout)
		}
		return fmt.Errorf("usage: co access request [options] <owner/repo> | co access wait [request-id] | co access view <owner/repo>")
	case "agent":
		if len(args) == 3 && args[1] == "register" {
			return registerAgent(args[2], stdout)
		}
		if len(args) == 2 && args[1] == "list" {
			return listAgents(stdout)
		}
		if len(args) >= 2 && args[1] == "attest" {
			return attestAgentCommits(args[2:], stdout)
		}
		return fmt.Errorf("usage: co agent register <name> | co agent list | co agent attest <owner/repo> <commit-oid>...")
	case "git-credential":
		return gitCredentialCommand(args[1:], stdin, stdout)
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
		_, err := fmt.Fprintln(stdout, partialHelp)
		return err
	default:
		return fmt.Errorf("command %q is not migrated to Go; use the retained Rust CLI", command)
	}
}
