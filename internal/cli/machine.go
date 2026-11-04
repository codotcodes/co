package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/term"
)

const machineUsage = "usage: co machine enroll --setup-key | --owner SLUG --label NAME | co machine list|heartbeat ID|run ID"
const machineEnrollUsage = "usage: co machine enroll --setup-key | --owner SLUG --label NAME"

type machineIdentity struct {
	ID         string `json:"id"`
	Scope      string `json:"scope"`
	API        string `json:"api_url"`
	Credential string `json:"credential"`
}

type machineEnrollCommand struct {
	SetupKey     bool
	Owner, Label *string
}

func parseMachineEnroll(args []string) (machineEnrollCommand, error) {
	command := machineEnrollCommand{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--setup-key" && !command.SetupKey:
			command.SetupKey = true
		case arg == "--owner" && command.Owner == nil:
			index++
			if index == len(args) {
				return command, errors.New("--owner requires a slug")
			}
			owner := args[index]
			command.Owner = &owner
		case arg == "--label" && command.Label == nil:
			index++
			if index == len(args) {
				return command, errors.New("--label requires a name")
			}
			label := args[index]
			command.Label = &label
		default:
			return command, errors.New(machineEnrollUsage)
		}
	}
	if command.SetupKey == (command.Owner != nil) || (command.Owner == nil) != (command.Label == nil) {
		return command, errors.New(machineEnrollUsage)
	}
	return command, nil
}

func machineCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(machineUsage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch {
	case args[0] == "enroll":
		command, err := parseMachineEnroll(args[1:])
		if err != nil {
			return err
		}
		return enrollMachine(ctx, command, stdin, stdout)
	case args[0] == "list" && len(args) == 1:
		config, err := loadConfig()
		if err != nil {
			return err
		}
		var machines []machineIdentity
		_ = json.Unmarshal(config["machines"], &machines)
		for _, machine := range machines {
			if _, err := fmt.Fprintf(stdout, "%s %s %s\n", machine.ID, machine.Scope, machine.API); err != nil {
				return err
			}
		}
		return nil
	case args[0] == "heartbeat" && len(args) == 2:
		_, err := heartbeatMachine(ctx, args[1])
		return err
	case args[0] == "run":
		return errors.New("machine run is not migrated to Go; use the retained Rust CLI")
	default:
		return errors.New(machineUsage)
	}
}

func machineResources() (map[string]any, error) {
	ram, err := machineRAMMiB()
	if err != nil {
		return nil, err
	}
	cpus := machineCPUCount()
	if cpus > 1024 || ram > 16_777_216 {
		return nil, errors.New("detected resources exceed supported limits")
	}
	platform, arch := runtime.GOOS, runtime.GOARCH
	if platform == "darwin" {
		platform = "macos"
	}
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	case "386":
		arch = "x86"
	}
	return map[string]any{"attributes": []string{platform, arch}, "vcpus": cpus, "ramMiB": ram}, nil
}

func readSetupKey(ctx context.Context, stdin io.Reader) (string, error) {
	if file, ok := stdin.(*os.File); ok {
		if descriptor, err := secretFileDescriptor(file); err == nil && term.IsTerminal(descriptor) {
			return readHiddenSecret(ctx, "Paste the setup key: ", 4096)
		}
	}
	var value strings.Builder
	for value.Len() <= 4096 {
		var one []byte
		var err error
		if file, ok := stdin.(*os.File); ok {
			one, err = readSecretFileContext(ctx, file, 0)
		} else {
			one, err = io.ReadAll(io.LimitReader(stdin, 1))
		}
		if err != nil {
			return "", errors.New("unable to read setup key")
		}
		if len(one) == 0 || one[0] == '\n' {
			return value.String(), nil
		}
		value.Write(one)
	}
	return "", errors.New("setup key exceeds 4096 bytes")
}

func enrollMachine(ctx context.Context, command machineEnrollCommand, stdin io.Reader, stdout io.Writer) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	api := effectiveAPI(config)
	resources, err := machineResources()
	if err != nil {
		return err
	}
	var credential *apiCredential
	var private []string
	path := "/machine-enrollment/redeem"
	if command.SetupKey {
		key, err := readSetupKey(ctx, stdin)
		if err != nil {
			return err
		}
		key = strings.TrimSpace(key)
		if !utf8.ValidString(key) || !strings.HasPrefix(key, "co_setup_") || !cleanCredential(key) {
			return errors.New("invalid setup key")
		}
		resources["setupKey"] = key
		private = append(private, key)
	} else {
		owner, label := *command.Owner, *command.Label
		if owner == "" || strings.Trim(owner, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			return errors.New("owner must be a lowercase slug")
		}
		if strings.TrimSpace(label) == "" || len(label) > 80 {
			return errors.New("label must be 1-80 characters")
		}
		if config.stringValue("session_token") == nil {
			return errors.New("not signed in; run `co login`")
		}
		credential, err = agentHumanCredential(config)
		if err != nil {
			return err
		}
		path = "/machines/owners/" + owner + "/self-register"
		resources["label"] = label
	}
	// The setup-key payload is sensitive even though it has no Authorization header.
	if credential == nil {
		credential = &apiCredential{scoped: true}
	}
	response, err := requestAPI(ctx, api, "POST", path, credential, resources)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, err := decodeSessionResponse(response)
		return redactAgentError(err, append(profileSecrets(config, ""), private...)...)
	}
	record, err := apiRecord(response, []string{"machineId", "scope", "credential"}, "invalid API response")
	if err != nil {
		return err
	}
	for _, key := range []string{"machineId", "scope", "credential"} {
		if err := configField(record, key, "string", "", false); err != nil {
			return errors.New("invalid enrollment response")
		}
	}
	machine := machineIdentity{ID: *record.stringValue("machineId"), Scope: *record.stringValue("scope"), API: api, Credential: *record.stringValue("credential")}
	if !validAgentIdentity(agentIdentity{ID: machine.ID, Name: "machine"}) || machine.Scope != "owner" && machine.Scope != "public" || !strings.HasPrefix(machine.Credential, "co_runner_") || !cleanCredential(machine.Credential) {
		return errors.New("invalid enrollment response")
	}
	if err := storeMachineEnrollment(ctx, config, machine, !command.SetupKey); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Machine %s enrolled. Run `co machine run %s` to connect it.\n", machine.ID, machine.ID)
	return err
}

func storeMachineEnrollment(ctx context.Context, original configuration, machine machineIdentity, human bool) error {
	return mutateConfigContext(ctx, func(current configuration) error {
		if !sameConfigString(original, current, "api_url") || human && !sameConfigString(original, current, "session_token") {
			return errors.New("profile changed during enrollment; no credential saved")
		}
		matching := func(config configuration) []configuration {
			var records []configuration
			_ = json.Unmarshal(config["machines"], &records)
			var matches []configuration
			for _, record := range records {
				if id := record.stringValue("id"); id != nil && *id == machine.ID {
					matches = append(matches, record)
				}
			}
			return matches
		}
		before, _ := json.Marshal(matching(original))
		after, _ := json.Marshal(matching(current))
		if string(before) != string(after) {
			return errors.New("machine enrollment changed concurrently; no credential saved")
		}
		var records []configuration
		_ = json.Unmarshal(current["machines"], &records)
		retained := make([]configuration, 0, len(records)+1)
		for _, record := range records {
			if id := record.stringValue("id"); id == nil || *id != machine.ID {
				retained = append(retained, record)
			}
		}
		raw, _ := json.Marshal(machine)
		var record configuration
		_ = json.Unmarshal(raw, &record)
		retained = append(retained, record)
		current["machines"], _ = json.Marshal(retained)
		return nil
	})
}

func selectMachine(config configuration, id string) (machineIdentity, error) {
	var records []machineIdentity
	_ = json.Unmarshal(config["machines"], &records)
	var selected *machineIdentity
	for _, machine := range records {
		if machine.ID == id {
			if selected != nil {
				return machine, errors.New("multiple local machine enrollments have this ID")
			}
			copy := machine
			selected = &copy
		}
	}
	if selected == nil {
		return machineIdentity{}, errors.New("machine is not enrolled in this CLI configuration")
	}
	if effectiveAPI(config) != selected.API {
		return machineIdentity{}, errors.New("machine was enrolled against a different API; select that API before running it")
	}
	if !strings.HasPrefix(selected.Credential, "co_runner_") || !cleanCredential(selected.Credential) {
		return machineIdentity{}, errors.New("invalid stored machine credential")
	}
	return *selected, nil
}

// Temporary failures retain the enrollment and may be retried by the connected runner.
func heartbeatMachine(ctx context.Context, id string) (bool, error) {
	config, err := loadConfig()
	if err != nil {
		return false, err
	}
	machine, err := selectMachine(config, id)
	if err != nil {
		return false, err
	}
	response, err := requestAPI(ctx, machine.API, "POST", "/machine-enrollment/heartbeat", &apiCredential{scheme: "Bearer", secret: machine.Credential, scoped: true}, nil)
	if err != nil {
		return true, err
	}
	response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return false, nil
	}
	if response.StatusCode == 401 {
		return false, errors.New("machine credential revoked or unavailable; enroll again")
	}
	temporary := response.StatusCode == 429 || response.StatusCode >= 500 && response.StatusCode <= 599
	return temporary, fmt.Errorf("machine heartbeat failed (HTTP %d %s)", response.StatusCode, http.StatusText(response.StatusCode))
}
