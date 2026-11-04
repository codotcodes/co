package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const secretSetUsage = "usage: co secret set <NAME> --owner <OWNER> | --repo <OWNER/REPO> [--value-file <PATH>] [--reauth-file <PATH>]"

type secretSetCommand struct {
	Name                       string
	Owner, Repo, Value, Reauth *string
}

func secretHelp(stdout io.Writer) error {
	_, err := fmt.Fprintf(stdout, `%s

Create or replace a workflow secret in a personal or repository scope.
Requires an unambiguous scope: --owner or --repo, never both.
Reads the secret value from an echo-disabled prompt, stdin, or --value-file.
Never pass a secret value as a command argument.

Options:
  --owner <OWNER>        personal or organization owner scope
  --repo <OWNER/REPO>    repository scope
  --value-file <PATH>    read the secret value from this file
  --reauth-file <PATH>   read password or TOTP from a separate private file
Stdin and value files are read in full, preserving every byte, including newlines.
Authentication is prompted on the terminal unless --reauth-file is given.

Examples:
  co secret set DATABASE_URL --owner my-org
  co secret set DATABASE_URL --repo my-org/my-app
  co secret set DATABASE_URL --owner my-org --value-file ./secret.txt
`, secretSetUsage)
	return err
}

func parseSecretSet(args []string) (secretSetCommand, error) {
	command := secretSetCommand{}
	hasName := false
	for index := 0; index < len(args); index++ {
		arg := args[index]
		var slot **string
		switch arg {
		case "--owner":
			slot = &command.Owner
		case "--repo":
			slot = &command.Repo
		case "--value-file":
			slot = &command.Value
		case "--reauth-file":
			slot = &command.Reauth
		default:
			if strings.HasPrefix(arg, "-") {
				return command, fmt.Errorf("unknown secret option %q\n%s", arg, secretSetUsage)
			}
			if hasName {
				return command, errors.New(secretSetUsage)
			}
			command.Name, hasName = arg, true
			continue
		}
		index++
		if index == len(args) {
			if arg == "--reauth-file" {
				return command, errors.New("--reauth-file requires a path")
			}
			return command, fmt.Errorf("%s requires a value\n%s", arg, secretSetUsage)
		}
		value := args[index]
		*slot = &value
	}
	if !hasName {
		return command, errors.New(secretSetUsage)
	}
	if err := validateSecretName(command.Name); err != nil {
		return command, err
	}
	if command.Owner != nil && command.Repo != nil {
		return command, errors.New("pass either --owner or --repo, not both")
	}
	if command.Owner == nil && command.Repo == nil {
		return command, fmt.Errorf("pass --owner <OWNER> or --repo <OWNER/REPO>\n%s", secretSetUsage)
	}
	if command.Owner != nil {
		if _, _, err := repositorySpec(*command.Owner + "/scope"); err != nil {
			return command, err
		}
	}
	return command, nil
}

func secretCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	help := func(args []string) bool {
		return len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help")
	}
	if help(args) || len(args) >= 1 && args[0] == "set" && help(args[1:]) {
		return secretHelp(stdout)
	}
	if len(args) == 0 {
		return errors.New("unknown secret command None (try: `co secret set --help`)")
	}
	if args[0] != "set" {
		return fmt.Errorf("unknown secret command Some(%q) (try: `co secret set --help`)", args[0])
	}
	command, err := parseSecretSet(args[1:])
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return setWorkflowSecret(ctx, command, stdin, stdout)
}

func secretResponse(response *http.Response, fields []string) (configuration, error) {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return apiRecord(response, fields, "invalid secret API response")
	}
	// Only allow server-defined error codes, never reflected values or proofs.
	code := "request_failed"
	if body, err := apiRecord(response, []string{"error"}, "invalid secret API response"); err == nil {
		if value := body.stringValue("error"); value != nil {
			for _, allowed := range []string{"unauthorized", "forbidden", "not_found", "version_conflict", "invalid_configuration", "configuration_limit", "invalid_reauthentication", "reauthentication_expired", "reauthentication_rate_limited", "reauthentication_method_unavailable", "workflow_secret_unavailable"} {
				if *value == allowed {
					code = allowed
				}
			}
		}
	}
	return nil, fmt.Errorf("secret request failed: %s (HTTP %d %s)", code, response.StatusCode, http.StatusText(response.StatusCode))
}

func setWorkflowSecret(ctx context.Context, command secretSetCommand, stdin io.Reader, stdout io.Writer) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	credential, err := agentHumanCredential(config)
	if err != nil {
		return err
	}
	base, label := "", ""
	if command.Owner != nil {
		base, label = "/workflow-config/owners/"+*command.Owner, "owner "+*command.Owner
	} else {
		owner, repo, err := repositorySpec(*command.Repo)
		if err != nil {
			return err
		}
		base, label = "/workflow-config/repos/"+owner+"/"+repo, "repository "+*command.Repo
	}
	request := func(method, path string, body any, fields []string) (configuration, error) {
		response, err := requestAPI(ctx, effectiveAPI(config), method, path, credential, body)
		if err != nil {
			return nil, err
		}
		return secretResponse(response, fields)
	}
	list, err := request("GET", base, nil, []string{"entries", "canManageSecrets", "reauthenticationMethods"})
	if err != nil {
		return err
	}
	if err := configField(list, "canManageSecrets", "bool", "", false); err != nil {
		return errors.New("invalid secret API response")
	}
	if string(list["canManageSecrets"]) != "true" {
		return fmt.Errorf("secret management on %s requires the owner role; run `co login` as the owner", label)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(list["entries"], &entries); err != nil || entries == nil {
		return errors.New("invalid secret API response")
	}
	version := uint64(0)
	foundVersion := false
	for _, raw := range entries {
		entry, err := configObject(raw, []string{"kind", "name", "version"})
		if err != nil {
			return errors.New("invalid secret API response")
		}
		for _, key := range []string{"kind", "name"} {
			if err := configField(entry, key, "string", "", false); err != nil {
				return errors.New("invalid secret API response")
			}
		}
		if err := configField(entry, "version", "uint", "", false); err != nil {
			return errors.New("invalid secret API response")
		}
		if *entry.stringValue("kind") == "secret" && *entry.stringValue("name") == command.Name && !foundVersion {
			_ = json.Unmarshal(entry["version"], &version)
			foundVersion = true
		}
	}
	if version == math.MaxUint64 {
		return errors.New("secret version cannot be incremented")
	}
	if err := configField(list, "reauthenticationMethods", "strings", "", false); err != nil {
		return errors.New("invalid secret API response")
	}
	var methods []string
	_ = json.Unmarshal(list["reauthenticationMethods"], &methods)
	method, err := selectReauthentication(methods)
	if err != nil {
		return err
	}
	value, err := readSecretValue(ctx, stdin, command.Value)
	if err != nil {
		return err
	}
	if err := validateSecretValue(value); err != nil {
		return err
	}
	challenge, err := request("POST", base+"/secret/"+command.Name+"/reauthentication", map[string]any{"action": "write", "version": version, "method": method}, []string{"challengeId"})
	if err != nil {
		return err
	}
	if err := configField(challenge, "challengeId", "string", "", false); err != nil || !cleanCredential(*challenge.stringValue("challengeId")) {
		return errors.New("invalid secret API response")
	}
	proof, err := reauthenticationProof(ctx, method, command.Reauth)
	if err != nil {
		return err
	}
	proof["challengeId"] = *challenge.stringValue("challengeId")
	result, err := request("PUT", base+"/secret/"+command.Name, map[string]any{"value": value, "version": version, "reauthentication": proof}, []string{"name", "kind", "version"})
	if err != nil {
		return err
	}
	for _, field := range []string{"name", "kind"} {
		if err := configField(result, field, "string", "", false); err != nil {
			return errors.New("invalid secret API response")
		}
	}
	var next uint64
	if err := configField(result, "version", "uint", "", false); err != nil {
		return errors.New("invalid secret API response")
	}
	_ = json.Unmarshal(result["version"], &next)
	if *result.stringValue("name") != command.Name || *result.stringValue("kind") != "secret" || next != version+1 {
		return errors.New("invalid secret API response")
	}
	_, err = fmt.Fprintf(stdout, "%s\n  kind    secret\n  version %d\n", command.Name, next)
	return err
}
