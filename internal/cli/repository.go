package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
)

const repoCreateUsage = "usage: co repo create [--public | --private] [--json] <[owner/]name>"

type createRepositoryCommand struct {
	Name       string  `json:"name"`
	Owner      *string `json:"orgSlug,omitempty"`
	Visibility string  `json:"visibility"`
	JSON       bool    `json:"-"`
}

func parseRepositoryCreate(args []string) (createRepositoryCommand, error) {
	command := createRepositoryCommand{Visibility: "private"}
	var spec *string
	visibility := ""
	for _, arg := range args {
		switch arg {
		case "--public", "--private":
			selected := strings.TrimPrefix(arg, "--")
			if visibility != "" && visibility != selected {
				return command, errors.New("choose either --public or --private")
			}
			visibility, command.Visibility = selected, selected
		case "--json":
			command.JSON = true
		default:
			if strings.HasPrefix(arg, "-") {
				return command, fmt.Errorf("unknown repository creation option %q\n%s", arg, repoCreateUsage)
			}
			if spec != nil {
				return command, errors.New(repoCreateUsage)
			}
			value := arg
			spec = &value
		}
	}
	if spec == nil {
		return command, errors.New(repoCreateUsage)
	}
	name := *spec
	if owner, repository, found := strings.Cut(*spec, "/"); found {
		if _, _, err := repositorySpec(owner + "/repo"); err != nil {
			return command, err
		}
		command.Owner, name = &owner, repository
	}
	invalid := errors.New("repository name must contain 1-128 letters, numbers, dots, underscores, or hyphens; it cannot start with _ or -, equal ., or contain consecutive dots")
	if len(name) < 1 || len(name) > 128 || name == "." || strings.Contains(name, "..") {
		return command, invalid
	}
	for index, character := range []byte(name) {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || index > 0 && (character == '_' || character == '-')) {
			return command, invalid
		}
	}
	command.Name = name
	return command, nil
}

func repositoryCommand(args []string, stdout, stderr io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) >= 1 {
		switch args[0] {
		case "view":
			if len(args) < 2 {
				return errors.New("usage: co repo view <owner/repo>")
			}
			return viewRepository(ctx, args[1], stdout)
		case "create":
			for _, arg := range args[1:] {
				if arg == "--help" || arg == "-h" {
					_, err := fmt.Fprintf(stdout, "%s\nCreate in your personal namespace by default, with private visibility.\nUse --json for machine-readable output. Requires `co login`.\n", repoCreateUsage)
					return err
				}
			}
			command, err := parseRepositoryCreate(args[1:])
			if err != nil {
				return err
			}
			return createRepository(ctx, command, stdout, stderr)
		}
	}
	return fmt.Errorf("usage: co repo view <owner/repo>\n%s", repoCreateUsage)
}

func viewRepository(ctx context.Context, spec string, stdout io.Writer) error {
	owner, name, err := repositorySpec(spec)
	if err != nil {
		return err
	}
	config, err := loadConfig()
	if err != nil {
		return err
	}
	var credential *apiCredential
	if config.stringValue("session_token") != nil {
		credential, err = agentHumanCredential(config)
		if err != nil {
			return err
		}
	}
	response, err := requestAPI(ctx, effectiveAPI(config), "GET", "/repos/"+owner+"/"+name, credential, nil)
	if err != nil {
		return err
	}
	body, err := decodeSessionResponse(response)
	secrets := profileSecrets(config, "")
	if err != nil {
		return redactAgentError(err, secrets...)
	}
	if privateJSON(body, secrets) {
		return errors.New("repository response contains private credential material")
	}
	object, _ := body.(map[string]any)
	field := func(key, fallback string) string {
		if value, ok := object[key].(string); ok {
			return value
		}
		return fallback
	}
	_, err = fmt.Fprintf(stdout, "%s/%s\n  visibility      %s\n  default branch  %s\n  web             https://co.codes/%s/%s\n", field("owner", owner), field("name", name), field("visibility", "unknown"), field("defaultBranch", "unknown"), owner, name)
	return err
}

func createRepository(ctx context.Context, command createRepositoryCommand, stdout, stderr io.Writer) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	credential, err := agentHumanCredential(config)
	if err != nil {
		return err
	}
	response, err := requestAPI(ctx, effectiveAPI(config), "POST", "/repos", credential, command)
	if err != nil {
		return fmt.Errorf("%w; creation may have completed; check the repository before retrying", err)
	}
	secrets := profileSecrets(config, "")
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, err := decodeSessionResponse(response)
		return redactAgentError(err, secrets...)
	}
	record, err := apiRecord(response, []string{"owner", "name", "visibility", "created"}, "invalid API response")
	if err != nil {
		return err
	}
	for _, key := range []string{"owner", "name", "visibility"} {
		if err := configField(record, key, "string", "", false); err != nil {
			return errors.New("invalid API response: invalid created repository")
		}
	}
	if err := configField(record, "created", "bool", "", false); err != nil {
		return errors.New("invalid API response: invalid repository initialization state")
	}
	raw, _ := json.Marshal(record)
	var public any
	_ = json.Unmarshal(raw, &public)
	if privateJSON(public, secrets) {
		return errors.New("repository response contains private credential material")
	}
	owner, name, visibility := *record.stringValue("owner"), *record.stringValue("name"), *record.stringValue("visibility")
	if command.JSON {
		rendered, err := renderCreatedRepository(record)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(stdout, string(rendered)); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(stdout, "Created %s/%s (%s)\n  web  https://co.codes/%s/%s\n  git  https://%s/%s/%s.git\n", owner, name, visibility, owner, name, gitHostForAPI(effectiveAPI(config)), owner, name); err != nil {
			return err
		}
	}
	if string(record["created"]) == "false" {
		_, err = fmt.Fprintln(stderr, "co: repository registered, but Git storage initialization was not confirmed; check the repository before pushing")
	}
	return err
}

func renderCreatedRepository(record configuration) ([]byte, error) {
	keys := []string{"owner", "name", "visibility", "created"}
	var extra []string
	for key := range record {
		if key != "owner" && key != "name" && key != "visibility" && key != "created" {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	keys = append(keys, extra...)
	var compact bytes.Buffer
	compact.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			compact.WriteByte(',')
		}
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(key)
		compact.Write(bytes.TrimSpace(encoded.Bytes()))
		compact.WriteByte(':')
		decoder := json.NewDecoder(bytes.NewReader(record[key]))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("unable to render repository response")
		}
		encoded.Reset()
		if err := encoder.Encode(value); err != nil {
			return nil, errors.New("unable to render repository response")
		}
		compact.Write(bytes.TrimSpace(encoded.Bytes()))
	}
	compact.WriteByte('}')
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, compact.Bytes(), "", "  "); err != nil {
		return nil, errors.New("unable to render repository response")
	}
	return pretty.Bytes(), nil
}
