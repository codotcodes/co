// Package cli contains the Go migration of the co command-line client.
package cli

import (
	"fmt"
	"io"
	"runtime"
)

const cliHelp = `co: the co.codes command line

  login                    authorize this machine
  logout                   revoke the local session
  whoami                   show the signed-in account
  repo view <owner/repo>   show repository metadata
  repo create [options] <[owner/]name>  create a repository
  agent register <name>    register a local agent identity
  agent list               list local agent identities
  agent attest <repo> <oid>...  attest commits using CO_AGENT_ID and a push grant
  access request [options] <repo>  request human-approved agent access
  access wait [request]    resume waiting for human approval
  access view <owner/repo> view a repo with an approved agent grant
  clone [options] <repo>   clone a repository over HTTPS
  link [options] <repo>    link the local Git repository
  git-credential <op>      serve credentials to Git
  machine enroll [options] enroll with a setup key or owner session
  machine run <id>        send heartbeats in the foreground
  machine heartbeat <id>  send one heartbeat
  workflow check [file]   validate local workflow plans
  deployment <command>   configure, approve and inspect native deployments
  secret set <name> --owner <owner> | --repo <owner/repo>  create or replace a workflow secret
  doctor                   check API and authentication
  version                  print version

Clone and link options:
  -u, --set-upstream-name <name>  set the Git remote name (default: origin)
      --jj / --no-jj              enable or disable colocated jj setup

Repository creation options:
      --public / --private        set visibility (default: private)
      --json                      print the API response as JSON

Access request options:
      --agent <id-or-name>        use or register this agent
      --push                      request pull and push (default: pull)
      --ttl <seconds>             grant lifetime, 300-2592000 (30 days; default: 3600)
      --reason <text>             explain the task to the approver
`

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	command := "help"
	if len(args) != 0 {
		command = args[0]
	}
	switch command {
	case "machine":
		return machineCommand(args[1:], stdin, stdout, stderr)
	case "doctor":
		return doctor(stdout)
	case "deployment":
		return runDeployment(args[1:], stdin, stdout)
	case "secret":
		return secretCommand(args[1:], stdin, stdout)
	case "clone", "link":
		return repositoryGit(args[1:], command == "clone", stdin, stdout, stderr)
	case "repo":
		return repositoryCommand(args[1:], stdout, stderr)
	case "access":
		if len(args) >= 2 && args[1] == "request" {
			return requestAgentAccess(args[2:], stdin, stdout, stderr)
		}
		if len(args) >= 2 && args[1] == "wait" && len(args) <= 3 {
			return waitAgentAccess(args[2:], stdout, stderr)
		}
		if len(args) == 3 && args[1] == "view" {
			return agentRepositoryView(args[2], stdout)
		}
		return fmt.Errorf("usage: co access request|wait|view")
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
		path := "~/.config/co/config.json"
		if runtime.GOOS == "windows" {
			path = `%APPDATA%\co\config.json`
		}
		_, err := fmt.Fprintf(stdout, "%s\nConfiguration: %s (or $XDG_CONFIG_HOME/co/config.json).\nSet CO_CONFIG_DIR to override only the CLI configuration directory.\nEnvironment: CO_API_URL overrides https://api.co.codes.\n", cliHelp, path)
		return err
	default:
		return fmt.Errorf("unknown command %q (try: co help)", command)
	}
}
