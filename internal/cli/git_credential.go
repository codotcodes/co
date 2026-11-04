package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unicode/utf8"
)

const credentialUsage = "usage: co git-credential get|store|erase"

type gitCredentialRequest struct {
	protocol, host string
	path           *string
}

func (request gitCredentialRequest) gitHost() string {
	if request.protocol != "https" {
		return ""
	}
	switch request.host {
	case "git.co.codes", "git.co.codes:443":
		return "git.co.codes"
	case "git.codevved.com", "git.codevved.com:443":
		return "git.codevved.com"
	}
	return ""
}

func gitHostForAPI(api string) string {
	switch strings.TrimRight(api, "/") {
	case "https://api.codevved.com", "https://api.codevved.com:443":
		return "git.codevved.com"
	default:
		return "git.co.codes"
	}
}

func parseGitCredential(input string) gitCredentialRequest {
	request := gitCredentialRequest{}
	lines := strings.Split(input, "\n")
	for index, line := range lines {
		if index < len(lines)-1 {
			line = strings.TrimSuffix(line, "\r")
		}
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			break
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch key {
		case "protocol":
			request.protocol = value
		case "host":
			request.host = value
		case "path":
			copy := value
			request.path = &copy
		}
	}
	return request
}

func gitCredentialCommand(args []string, stdin io.Reader, stdout io.Writer) (result error) {
	_, agentMode := os.LookupEnv("CO_AGENT_ID")
	get := len(args) == 1 && args[0] == "get"
	defer func() {
		if agentMode && result != nil && (get || len(args) != 1) {
			fmt.Fprint(stdout, "quit=true\n\n")
		}
	}()
	if len(args) != 1 || args[0] != "get" && args[0] != "store" && args[0] != "erase" {
		return errors.New(credentialUsage)
	}
	data, err := io.ReadAll(io.LimitReader(stdin, (1<<20)+1))
	if err != nil {
		return fmt.Errorf("unable to read Git credential request: %s", processError(err))
	}
	if len(data) > 1<<20 {
		return errors.New("Git credential request is too large")
	}
	if !utf8.Valid(data) {
		return errors.New("unable to read Git credential request: stream did not contain valid UTF-8")
	}
	if !get {
		return nil
	}
	request := parseGitCredential(string(data))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var token *string
	if agentMode {
		agent, err := selectedAgentID()
		if err != nil {
			return err
		}
		host := request.gitHost()
		if host == "" {
			return errors.New("agent credential request is outside supported HTTPS Git hosts")
		}
		if api, present := os.LookupEnv("CO_API_URL"); present && host != gitHostForAPI(api) {
			return fmt.Errorf("agent credential request is outside %s", gitHostForAPI(api))
		}
		config, err := loadConfig()
		if err != nil {
			return err
		}
		if host != gitHostForAPI(effectiveAPI(config)) {
			return fmt.Errorf("agent credential request is outside %s", gitHostForAPI(effectiveAPI(config)))
		}
		if request.path == nil {
			return errors.New("agent credentials require credential.useHttpPath=true; run co link again")
		}
		owner, repo, err := repositorySpec(strings.TrimSuffix(*request.path, ".git"))
		if err != nil {
			return err
		}
		minted, err := mintAgentToken(ctx, config, owner, repo, agent, "push")
		if err != nil {
			return err
		}
		token = &minted
	} else if host := request.gitHost(); host != "" {
		config, err := loadConfig()
		if err != nil {
			return err
		}
		if host == gitHostForAPI(effectiveAPI(config)) {
			token = config.stringValue("session_token")
		}
	}
	if token != nil {
		if !cleanCredential(*token) {
			return errors.New("invalid Git credential response")
		}
		_, err = fmt.Fprintf(stdout, "username=co\npassword=%s\n\n", *token)
		if err != nil {
			return fmt.Errorf("unable to write Git credential response: %s", processError(err))
		}
	}
	return nil
}
