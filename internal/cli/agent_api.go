package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

func agentRepositoryView(spec string, stdout io.Writer) error {
	owner, repo, err := repositorySpec(spec)
	if err != nil {
		return err
	}
	agent, err := selectedAgentID()
	if err != nil {
		return err
	}
	config, err := loadConfig()
	if err != nil {
		return err
	}
	token, err := mintAgentToken(context.Background(), config, owner, repo, agent, "pull")
	if err != nil {
		return err
	}
	response, err := requestAPI(context.Background(), effectiveAPI(config), "GET", "/t:"+url.PathEscape(token)+"/"+owner+"/"+repo, &apiCredential{scoped: true}, nil)
	if err != nil {
		if err.Error() == "request timed out" {
			return errors.New("agent repository request timed out")
		}
		return errors.New("agent repository request failed")
	}
	secrets := profileSecrets(config, token)
	body, err := decodeSessionResponse(response)
	if err != nil {
		return redactAgentError(err, secrets...)
	}
	if object, ok := body.(map[string]any); ok {
		if urls, ok := object["urls"].(map[string]any); ok {
			delete(urls, "self")
		}
	}
	if privateJSON(body, secrets) {
		return errors.New("agent repository response contains private credential material")
	}
	var rendered bytes.Buffer
	encoder := json.NewEncoder(&rendered)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		return errors.New("unable to render repository response")
	}
	_, err = stdout.Write(rendered.Bytes())
	return err
}

func attestAgentCommits(args []string, stdout io.Writer) error {
	if len(args) < 2 || len(args) > 101 {
		return errors.New("usage: CO_AGENT_ID=<id> co agent attest <owner/repo> <commit-oid>... (up to 100 explicit commits)")
	}
	owner, repo, err := repositorySpec(args[0])
	if err != nil {
		return err
	}
	for _, oid := range args[1:] {
		if len(oid) != 40 && len(oid) != 64 {
			return errors.New("attestation requires full 40- or 64-character commit object IDs")
		}
		for _, character := range []byte(oid) {
			if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f' || character >= 'A' && character <= 'F') {
				return errors.New("attestation requires full 40- or 64-character commit object IDs")
			}
		}
	}
	agent, err := selectedAgentID()
	if err != nil {
		return err
	}
	if agent == nil {
		return errors.New("set CO_AGENT_ID to the registered agent that worked on these commits")
	}
	config, err := loadConfig()
	if err != nil {
		return err
	}
	token, err := mintAgentToken(context.Background(), config, owner, repo, agent, "push")
	if err != nil {
		return err
	}
	secrets := profileSecrets(config, token)
	for _, oid := range args[1:] {
		oid = strings.ToLower(oid)
		response, err := requestAPI(context.Background(), effectiveAPI(config), "POST", "/repos/"+owner+"/"+repo+"/commits/"+oid+"/provenance", &apiCredential{scheme: "Bearer", secret: token, scoped: true}, nil)
		if err != nil {
			return err
		}
		if _, err := decodeSessionResponse(response); err != nil {
			return redactAgentError(err, secrets...)
		}
		if _, err := fmt.Fprintf(stdout, "Attested %s as %s\n", oid, *agent); err != nil {
			return err
		}
	}
	return nil
}

func profileSecrets(config configuration, token string) []string {
	secrets := []string{token}
	if value := config.stringValue("session_token"); value != nil {
		secrets = append(secrets, *value)
	}
	for _, field := range []struct{ list, key string }{{"agent_grants", "lineage_token"}, {"pending_agent_requests", "poll_token"}, {"pending_agent_submissions", "request_capability"}, {"machines", "credential"}} {
		var records []configuration
		_ = json.Unmarshal(config[field.list], &records)
		for _, record := range records {
			if value := record.stringValue(field.key); value != nil {
				secrets = append(secrets, *value)
			}
		}
	}
	return secrets
}

func privateJSON(value any, secrets []string) bool {
	switch data := value.(type) {
	case string:
		for _, secret := range secrets {
			if secret != "" && (strings.Contains(data, secret) || strings.Contains(data, url.PathEscape(secret)) || strings.Contains(data, url.QueryEscape(secret))) {
				return true
			}
		}
	case []any:
		for _, item := range data {
			if privateJSON(item, secrets) {
				return true
			}
		}
	case map[string]any:
		for key, item := range data {
			if privateJSON(key, secrets) || privateJSON(item, secrets) {
				return true
			}
		}
	}
	return false
}
