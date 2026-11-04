package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
)

type agentGrant struct {
	AgentID      string   `json:"agent_id"`
	Owner        string   `json:"owner"`
	Repo         string   `json:"repo"`
	GrantID      string   `json:"grant_id"`
	LineageToken string   `json:"lineage_token"`
	Operations   []string `json:"operations"`
	Expires      uint64   `json:"expires_unix"`
	API          *string  `json:"api_url"`
}

func selectedAgentID() (*string, error) {
	id, present := os.LookupEnv("CO_AGENT_ID")
	if !present {
		return nil, nil
	}
	if len(id) == 0 || len(id) > 64 {
		return nil, errors.New("CO_AGENT_ID must be a registered agent ID")
	}
	for _, value := range []byte(id) {
		if !(value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '-') {
			return nil, errors.New("CO_AGENT_ID must be a registered agent ID")
		}
	}
	return &id, nil
}

func selectAgentGrant(config configuration, owner, repo string, agent *string, operation string, now uint64) (agentGrant, error) {
	var grants []agentGrant
	if err := json.Unmarshal(config["agent_grants"], &grants); err != nil {
		return agentGrant{}, errors.New("invalid stored agent grant")
	}
	var selected *agentGrant
	for _, grant := range grants {
		if grant.Owner != owner || grant.Repo != repo || agent != nil && grant.AgentID != *agent || grant.Expires <= now {
			continue
		}
		allowed := false
		for _, op := range grant.Operations {
			if op == operation {
				allowed = true
			}
		}
		if !allowed {
			continue
		}
		if selected != nil {
			return agentGrant{}, fmt.Errorf("multiple live agent grants for %s/%s; use a separate local co config per agent", owner, repo)
		}
		copy := grant
		selected = &copy
	}
	if selected == nil {
		return agentGrant{}, fmt.Errorf("no live %s grant for %s/%s; request access for the selected agent with `co access request --agent <id> --push %s/%s`", operation, owner, repo, owner, repo)
	}
	return *selected, nil
}

func cleanCredential(value string) bool {
	if len(value) == 0 || len(value) > 64<<10 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func mintAgentToken(ctx context.Context, config configuration, owner, repo string, agent *string, operation string) (string, error) {
	now := time.Now().Unix()
	if now < 0 {
		return "", errors.New("system clock is before Unix epoch")
	}
	grant, err := selectAgentGrant(config, owner, repo, agent, operation, uint64(now))
	if err != nil {
		return "", err
	}
	api := effectiveAPI(config)
	bound := "https://api.co.codes"
	if value := config.stringValue("api_url"); value != nil {
		bound = strings.TrimRight(*value, "/")
	}
	if api != bound || grant.API != nil && strings.TrimRight(*grant.API, "/") != api {
		return "", errors.New("agent grant API does not match this profile; use the matching CO_CONFIG_DIR and CO_API_URL")
	}
	if grant.GrantID == "" || grant.GrantID == "." || grant.GrantID == ".." || strings.ContainsAny(grant.GrantID, "/\\?#%") || !cleanCredential(grant.GrantID) || !cleanCredential(grant.LineageToken) {
		return "", errors.New("invalid stored agent grant")
	}
	response, err := requestAPI(ctx, api, "POST", "/grants/"+grant.GrantID+"/token", &apiCredential{scheme: "Lineage", secret: grant.LineageToken, scoped: true}, nil)
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, err := decodeSessionResponse(response)
		return "", redactAgentError(err, grant.LineageToken)
	}
	record, err := apiRecord(response, []string{"accessToken"}, "invalid API response")
	if err != nil {
		return "", err
	}
	if err := configField(record, "accessToken", "string", "", false); err != nil {
		return "", errors.New("invalid API response: invalid agent access token")
	}
	token := record.stringValue("accessToken")
	if token == nil || !cleanCredential(*token) {
		return "", errors.New("invalid API response: invalid agent access token")
	}
	return *token, nil
}

func redactAgentError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			for _, form := range []string{secret, url.PathEscape(secret), url.QueryEscape(secret)} {
				message = strings.ReplaceAll(message, form, "[redacted]")
			}
		}
	}
	return errors.New(message)
}

func repositorySpec(spec string) (string, string, error) {
	invalid := errors.New("repository must be written as <owner/repo>")
	owner, repo, found := strings.Cut(spec, "/")
	if !found || len(owner) < 2 || len(owner) > 64 || len(repo) < 1 || len(repo) > 128 || repo == "." || repo == ".." {
		return "", "", invalid
	}
	for index, character := range []byte(owner) {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || index > 0 && character == '-') {
			return "", "", invalid
		}
	}
	for index, character := range []byte(repo) {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && strings.ContainsRune("._-", rune(character))) {
			return "", "", invalid
		}
	}
	return owner, repo, nil
}
