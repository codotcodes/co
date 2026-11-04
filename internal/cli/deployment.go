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

const deploymentUsage = "usage: co deployment list OWNER/REPO\n       co deployment configure OWNER/REPO ENV --file POLICY.json [--reauth-file PATH]\n       co deployment set-secret OWNER/REPO NAME [--environment ENV] [--value-file PATH] [--reauth-file PATH]\n       co deployment approve OWNER/REPO ID [--reauth-file PATH]\n       co deployment rollback OWNER/REPO ID [--reauth-file PATH]"

type deploymentCommand struct {
	Action, Owner, Repo, Subject string
	Policy, Value, Reauth, Env   *string
}

func validDeploymentEnvironment(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range []byte(value) {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-') {
			return false
		}
	}
	return true
}

func validDeploymentID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range []byte(value) {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
		} else if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f' || character >= 'A' && character <= 'F') {
			return false
		}
	}
	return true
}

func parseDeployment(args []string) (deploymentCommand, error) {
	command := deploymentCommand{}
	usage := errors.New(deploymentUsage)
	if len(args) < 2 {
		return command, usage
	}
	command.Action = args[0]
	switch command.Action {
	case "list", "configure", "set-secret", "approve", "rollback":
	default:
		return command, usage
	}
	var err error
	command.Owner, command.Repo, err = repositorySpec(args[1])
	if err != nil {
		return command, err
	}
	index := 2
	if command.Action != "list" {
		if len(args) < 3 {
			return command, usage
		}
		command.Subject, index = args[2], 3
	}
	for index < len(args) {
		var slot **string
		switch {
		case args[index] == "--file" && command.Action == "configure":
			slot = &command.Policy
		case args[index] == "--value-file" && command.Action == "set-secret":
			slot = &command.Value
		case args[index] == "--reauth-file" && command.Action != "list":
			slot = &command.Reauth
		case args[index] == "--environment" && command.Action == "set-secret":
			slot = &command.Env
		default:
			return command, usage
		}
		if *slot != nil {
			return command, errors.New("duplicate deployment option")
		}
		index++
		if index == len(args) {
			return command, usage
		}
		value := args[index]
		*slot = &value
		index++
	}
	if command.Action == "configure" && (!validDeploymentEnvironment(command.Subject) || command.Policy == nil) {
		return command, usage
	}
	if command.Action == "set-secret" {
		if err := validateSecretName(command.Subject); err != nil {
			return command, err
		}
		if command.Env != nil && !validDeploymentEnvironment(*command.Env) {
			return command, errors.New("invalid deployment environment")
		}
	}
	if (command.Action == "approve" || command.Action == "rollback") && !validDeploymentID(command.Subject) {
		return command, errors.New("invalid deployment ID")
	}
	return command, nil
}

func runDeployment(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		_, err := fmt.Fprintln(stdout, deploymentUsage)
		return err
	}
	command, err := parseDeployment(args)
	if err != nil {
		return err
	}
	if _, present := os.LookupEnv("CO_AGENT_ID"); present {
		return errors.New("deployment management requires an explicit human session, not agent mode")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return manageDeployment(ctx, command, stdin, stdout)
}

func deploymentJSON(response *http.Response) (any, error) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, fmt.Errorf("deployment request failed (HTTP %d %s)", response.StatusCode, http.StatusText(response.StatusCode))
	}
	value, err := decodeSessionResponse(response)
	if err != nil {
		return nil, errors.New("invalid deployment API response")
	}
	return value, nil
}

func manageDeployment(ctx context.Context, command deploymentCommand, stdin io.Reader, stdout io.Writer) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	credential, err := agentHumanCredential(config)
	if err != nil {
		return err
	}
	base := "/repos/" + command.Owner + "/" + command.Repo
	request := func(method, path string, body any) (any, error) {
		response, err := requestAPI(ctx, effectiveAPI(config), method, path, credential, body)
		if err != nil {
			return nil, err
		}
		return deploymentJSON(response)
	}
	if command.Action == "list" {
		result, err := request("GET", base+"/deployments", nil)
		if err != nil {
			return err
		}
		if privateJSON(result, profileSecrets(config, "")) {
			// Retain the existing session redaction, and also refuse other credentials.
			result = redactDeploymentJSON(result, credential.secret)
			if privateJSON(result, profileSecrets(config, "")) {
				return errors.New("deployment list contains private credential material")
			}
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		return encoder.Encode(result)
	}
	current, err := request("GET", base+"/deployment-config", nil)
	if err != nil {
		return err
	}
	fields, _ := current.(map[string]any)
	rawMethods, ok := fields["reauthenticationMethods"].([]any)
	if !ok {
		return errors.New("invalid deployment configuration response")
	}
	var methods []string
	for _, value := range rawMethods {
		if method, ok := value.(string); ok {
			methods = append(methods, method)
		}
	}
	method, err := selectReauthentication(methods)
	if err != nil {
		return err
	}
	proof := func(path string, body any) (map[string]any, error) {
		challenge, err := request("POST", path, body)
		if err != nil {
			return nil, err
		}
		object, _ := challenge.(map[string]any)
		id, _ := object["challengeId"].(string)
		if !cleanCredential(id) {
			return nil, errors.New("invalid authentication challenge")
		}
		proof, err := reauthenticationProof(ctx, method, command.Reauth)
		if err != nil {
			return nil, err
		}
		proof["challengeId"] = id
		return proof, nil
	}
	if command.Action == "approve" || command.Action == "rollback" {
		path := base + "/deployments/" + command.Subject
		auth, err := proof(path+"/reauthentication", map[string]any{"action": command.Action, "method": method})
		if err != nil {
			return err
		}
		result, err := request("POST", path+"/"+command.Action, map[string]any{"reauthentication": auth})
		if err != nil {
			return err
		}
		object, _ := result.(map[string]any)
		id, _ := object["id"].(string)
		if !validDeploymentID(id) {
			return errors.New("invalid deployment receipt")
		}
		if command.Action == "approve" && (id != command.Subject || object["approved"] != true) {
			return errors.New("invalid deployment approval receipt")
		}
		_, err = fmt.Fprintln(stdout, id)
		return err
	}
	environments, ok := fields["environments"].([]any)
	if !ok {
		return errors.New("invalid deployment configuration response")
	}
	var operation map[string]any
	var value *string
	version := uint64(0)
	if command.Action == "configure" {
		file, err := os.Open(*command.Policy)
		if err != nil {
			return errors.New("unable to open deployment policy")
		}
		content, err := readBoundedSecret(ctx, file, 100_000)
		file.Close()
		if err != nil {
			if strings.Contains(err.Error(), "exceeds") {
				return errors.New("deployment policy exceeds 100000 bytes")
			}
			return errors.New("unable to read deployment policy")
		}
		var policy any
		decoder := json.NewDecoder(strings.NewReader(content))
		decoder.UseNumber()
		if err := decoder.Decode(&policy); err != nil {
			return errors.New("invalid deployment policy JSON")
		}
		if !json.Valid([]byte(content)) {
			return errors.New("invalid deployment policy JSON")
		}
		for _, entry := range environments {
			if object, ok := entry.(map[string]any); ok && object["name"] == command.Subject {
				version = jsonUint(object["version"])
				break
			}
		}
		operation = map[string]any{"action": "configure-environment", "environment": command.Subject, "version": version, "config": policy}
	} else {
		var environmentID any
		if command.Env != nil {
			for _, entry := range environments {
				if object, ok := entry.(map[string]any); ok && object["name"] == *command.Env {
					if id, ok := object["id"].(string); ok {
						environmentID = id
					}
					break
				}
			}
			if environmentID == nil {
				return errors.New("deployment environment does not exist")
			}
		}
		secrets, ok := fields["secrets"].([]any)
		if !ok {
			return errors.New("invalid deployment configuration response")
		}
		for _, entry := range secrets {
			if object, ok := entry.(map[string]any); ok && object["name"] == command.Subject && object["environmentId"] == environmentID {
				version = jsonUint(object["version"])
				break
			}
		}
		operation = map[string]any{"action": "write-secret", "environment": command.Env, "name": command.Subject, "version": version}
		input, err := readSecretValue(ctx, stdin, command.Value)
		if err != nil {
			return err
		}
		if err := validateSecretValue(input); err != nil {
			return err
		}
		value = &input
	}
	if version == math.MaxUint64 {
		return errors.New("deployment version cannot be incremented")
	}
	auth, err := proof(base+"/deployment-config/reauthentication", map[string]any{"operation": operation, "method": method})
	if err != nil {
		return err
	}
	body := map[string]any{"operation": operation, "reauthentication": auth}
	if value != nil {
		body["value"] = *value
	}
	result, err := request("POST", base+"/deployment-config/apply", body)
	if err != nil {
		return err
	}
	object, _ := result.(map[string]any)
	var expectedEnvironment any = command.Env
	if command.Action == "configure" {
		expectedEnvironment = command.Subject
	} else if command.Env != nil {
		expectedEnvironment = *command.Env
	} else {
		expectedEnvironment = nil
	}
	if object["action"] != operation["action"] || object["environment"] != expectedEnvironment || jsonUint(object["version"]) != version+1 {
		return errors.New("invalid deployment configuration receipt")
	}
	_, err = fmt.Fprintln(stdout, command.Subject)
	return err
}

func jsonUint(value any) uint64 {
	if number, ok := value.(json.Number); ok {
		var result uint64
		if err := json.Unmarshal([]byte(number), &result); err == nil {
			return result
		}
	}
	return 0
}

func redactDeploymentJSON(value any, secret string) any {
	switch data := value.(type) {
	case string:
		if secret != "" {
			return strings.ReplaceAll(data, secret, "***")
		}
	case []any:
		for index, item := range data {
			data[index] = redactDeploymentJSON(item, secret)
		}
	case map[string]any:
		redacted := make(map[string]any, len(data))
		for key, item := range data {
			redacted[redactDeploymentJSON(key, secret).(string)] = redactDeploymentJSON(item, secret)
		}
		return redacted
	}
	return value
}
