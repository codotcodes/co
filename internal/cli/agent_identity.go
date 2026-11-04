package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

type agentIdentity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func listAgents(stdout io.Writer) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	var agents []agentIdentity
	if err := json.Unmarshal(config["agents"], &agents); err != nil {
		return errors.New("invalid stored agents")
	}
	if len(agents) == 0 {
		_, err = fmt.Fprintln(stdout, "No local agents. Run `co agent register <name>`.")
		return err
	}
	for _, agent := range agents {
		if _, err := fmt.Fprintf(stdout, "%s\n  id  %s\n", agent.Name, agent.ID); err != nil {
			return err
		}
	}
	return nil
}

func registerAgent(name string, stdout io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config, err := loadConfig()
	if err != nil {
		return err
	}
	agent, err := registerAgentRemote(ctx, config, name)
	if err != nil {
		return err
	}
	if err := rememberAgent(ctx, config, agent); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s\n  id  %s\n", agent.Name, agent.ID)
	return err
}

func profileAPI(config configuration) string {
	if value := config.stringValue("api_url"); value != nil {
		return strings.TrimRight(*value, "/")
	}
	return "https://api.co.codes"
}

func agentHumanCredential(config configuration) (*apiCredential, error) {
	token := config.stringValue("session_token")
	if token == nil {
		return nil, errors.New("not signed in; run `co login` and have a human authorize this machine")
	}
	if effectiveAPI(config) != profileAPI(config) {
		return nil, errors.New("profile contains credentials for another API; use a separate CO_CONFIG_DIR")
	}
	return &apiCredential{scheme: "Bearer", secret: *token, scoped: true}, nil
}

func registerAgentRemote(ctx context.Context, config configuration, name string) (agentIdentity, error) {
	name = strings.TrimSpace(name)
	if len(name) == 0 || len(name) > 80 {
		return agentIdentity{}, errors.New("agent name must contain 1 to 80 characters")
	}
	credential, err := agentHumanCredential(config)
	if err != nil {
		return agentIdentity{}, err
	}
	response, err := requestAPI(ctx, effectiveAPI(config), "POST", "/agents", credential, map[string]string{"name": name})
	if err != nil {
		return agentIdentity{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, err := decodeSessionResponse(response)
		return agentIdentity{}, redactAgentError(err, profileSecrets(config, "")...)
	}
	record, err := apiRecord(response, []string{"id", "name"}, "invalid API response")
	if err != nil {
		return agentIdentity{}, err
	}
	for _, field := range []string{"id", "name"} {
		if err := configField(record, field, "string", "", false); err != nil {
			return agentIdentity{}, errors.New("invalid API response: invalid agent identity")
		}
	}
	agent := agentIdentity{ID: *record.stringValue("id"), Name: *record.stringValue("name")}
	if !validAgentIdentity(agent) || privateJSON(map[string]any{"id": agent.ID, "name": agent.Name}, profileSecrets(config, "")) {
		return agentIdentity{}, errors.New("invalid API response: invalid agent identity")
	}
	return agent, nil
}

func validAgentIdentity(agent agentIdentity) bool {
	if len(agent.ID) == 0 || len(agent.ID) > 64 || len(agent.Name) == 0 || len(agent.Name) > 80 {
		return false
	}
	for _, character := range []byte(agent.ID) {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-') {
			return false
		}
	}
	return true
}

func rememberAgent(ctx context.Context, original configuration, agent agentIdentity) error {
	return mutateConfigContext(ctx, func(current configuration) error {
		if !sameConfigString(original, current, "session_token") || !sameConfigString(original, current, "api_url") {
			return errors.New("profile changed during agent registration; no identity saved")
		}
		var records []configuration
		if err := json.Unmarshal(current["agents"], &records); err != nil {
			return errors.New("invalid stored agents")
		}
		for _, existing := range records {
			if id := existing.stringValue("id"); id != nil && *id == agent.ID {
				return nil
			}
		}
		raw, _ := json.Marshal(agent)
		var record configuration
		_ = json.Unmarshal(raw, &record)
		records = append(records, record)
		current["agents"], _ = json.Marshal(records)
		return nil
	})
}
