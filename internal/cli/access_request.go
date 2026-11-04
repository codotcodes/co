package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const accessRequestUsage = "usage: co access request [--agent <id-or-name>] [--push] [--ttl <seconds>] [--reason <text>] <owner/repo>"

type accessRequestCommand struct {
	Spec   string
	Agent  *string
	Push   bool
	TTL    uint64
	Reason *string
}

type agentSubmission struct {
	AgentID     string   `json:"agent_id"`
	Owner       string   `json:"owner"`
	Repo        string   `json:"repo"`
	Operations  []string `json:"operations"`
	TTL         uint64   `json:"ttl_seconds"`
	Reason      *string  `json:"reason"`
	Capability  string   `json:"request_capability"`
	Idempotency string   `json:"idempotency_key"`
}

func parseAccessRequest(args []string) (accessRequestCommand, error) {
	command := accessRequestCommand{TTL: 3600}
	hasSpec := false
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "--agent", "--reason", "--ttl":
			index++
			if index == len(args) {
				return command, fmt.Errorf("%s requires a value\n%s", arg, accessRequestUsage)
			}
			value := args[index]
			switch arg {
			case "--agent":
				command.Agent = &value
			case "--reason":
				command.Reason = &value
			case "--ttl":
				var err error
				command.TTL, err = strconv.ParseUint(strings.TrimPrefix(value, "+"), 10, 64)
				if err != nil || strings.HasPrefix(value, "++") {
					return command, fmt.Errorf("--ttl must be an integer\n%s", accessRequestUsage)
				}
				if command.TTL < 300 || command.TTL > 2_592_000 {
					return command, errors.New("--ttl must be between 300 and 2592000 seconds (30 days)")
				}
			}
		case "--push":
			command.Push = true
		default:
			if strings.HasPrefix(arg, "-") {
				return command, fmt.Errorf("unknown access option %q\n%s", arg, accessRequestUsage)
			}
			if hasSpec {
				return command, errors.New(accessRequestUsage)
			}
			command.Spec, hasSpec = arg, true
		}
	}
	if !hasSpec {
		return command, errors.New(accessRequestUsage)
	}
	if _, _, err := repositorySpec(command.Spec); err != nil {
		return command, err
	}
	if command.Reason != nil && utf8.RuneCountInString(*command.Reason) > 500 {
		return command, errors.New("--reason must be 500 characters or fewer")
	}
	return command, nil
}

func resolveRequestAgent(ctx context.Context, config configuration, selector *string, stdout io.Writer) (agentIdentity, error) {
	var local []agentIdentity
	_ = json.Unmarshal(config["agents"], &local)
	selectAgent := func(agents []agentIdentity) (*agentIdentity, error) {
		for _, agent := range agents {
			if agent.ID == *selector {
				return &agent, nil
			}
		}
		var selected *agentIdentity
		for _, agent := range agents {
			if agent.Name == *selector {
				if selected != nil {
					return nil, fmt.Errorf("multiple agents are named %q; pass an agent ID", *selector)
				}
				copy := agent
				selected = &copy
			}
		}
		return selected, nil
	}
	if selector == nil {
		switch len(local) {
		case 0:
			return agentIdentity{}, errors.New("no local agent; pass `--agent <name>` to register one")
		case 1:
			if !validAgentIdentity(local[0]) {
				return agentIdentity{}, errors.New("invalid stored agent identity")
			}
			return local[0], nil
		default:
			return agentIdentity{}, errors.New("multiple local agents; pass `--agent <id-or-name>`")
		}
	}
	if selected, err := selectAgent(local); err != nil {
		return agentIdentity{}, err
	} else if selected != nil {
		if !validAgentIdentity(*selected) {
			return agentIdentity{}, errors.New("invalid stored agent identity")
		}
		return *selected, nil
	}
	credential, err := agentHumanCredential(config)
	if err != nil {
		return agentIdentity{}, err
	}
	response, err := requestAPI(ctx, effectiveAPI(config), "GET", "/agents", credential, nil)
	if err != nil {
		return agentIdentity{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, err := decodeSessionResponse(response)
		return agentIdentity{}, redactAgentError(err, profileSecrets(config, "")...)
	}
	record, err := apiRecord(response, []string{"agents"}, "invalid API response")
	if err != nil {
		return agentIdentity{}, err
	}
	var rawAgents []json.RawMessage
	if err := json.Unmarshal(record["agents"], &rawAgents); err != nil || rawAgents == nil {
		return agentIdentity{}, errors.New("invalid API response: invalid agent list")
	}
	var remote []agentIdentity
	for _, raw := range rawAgents {
		fields, err := configObject(raw, []string{"id", "name"})
		if err != nil {
			return agentIdentity{}, errors.New("invalid API response: invalid agent identity")
		}
		for _, key := range []string{"id", "name"} {
			if err := configField(fields, key, "string", "", false); err != nil {
				return agentIdentity{}, errors.New("invalid API response: invalid agent identity")
			}
		}
		agent := agentIdentity{ID: *fields.stringValue("id"), Name: *fields.stringValue("name")}
		if !validAgentIdentity(agent) || privateJSON(map[string]any{"id": agent.ID, "name": agent.Name}, profileSecrets(config, "")) {
			return agentIdentity{}, errors.New("invalid API response: invalid agent identity")
		}
		remote = append(remote, agent)
	}
	selected, err := selectAgent(remote)
	if err != nil {
		return agentIdentity{}, err
	}
	registered := selected == nil
	if registered {
		agent, err := registerAgentRemote(ctx, config, *selector)
		if err != nil {
			return agentIdentity{}, err
		}
		selected = &agent
	}
	if err := rememberAgent(ctx, config, *selected); err != nil {
		return agentIdentity{}, err
	}
	if registered {
		if _, err := fmt.Fprintf(stdout, "Registered agent %s (%s)\n", selected.Name, selected.ID); err != nil {
			return agentIdentity{}, err
		}
	}
	return *selected, nil
}

func requestAgentAccess(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	command, err := parseAccessRequest(args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return submitAgentAccess(ctx, command, stdout, stderr, func(ctx context.Context, target string) error {
		return openBrowser(ctx, target, stdin, stdout, stderr)
	}, accessRuntime{wait: waitForDevice, now: time.Now})
}

func submitAgentAccess(ctx context.Context, command accessRequestCommand, stdout, stderr io.Writer, open func(context.Context, string) error, run accessRuntime) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	if effectiveAPI(config) != profileAPI(config) {
		return errors.New("agent request API does not match this profile; use the matching CO_CONFIG_DIR and CO_API_URL")
	}
	agent, err := resolveRequestAgent(ctx, config, command.Agent, stdout)
	if err != nil {
		return err
	}
	owner, repo, err := repositorySpec(command.Spec)
	if err != nil {
		return err
	}
	operations := []string{"pull"}
	if command.Push {
		operations = append(operations, "push")
	}
	desired := agentSubmission{AgentID: agent.ID, Owner: owner, Repo: repo, Operations: operations, TTL: command.TTL, Reason: command.Reason}
	submission, err := prepareAgentSubmission(ctx, config, desired)
	if err != nil {
		return err
	}
	response, err := requestAPIHeaders(ctx, effectiveAPI(config), "POST", "/grant-requests", &apiCredential{scheme: "Agent-Request", secret: submission.Capability, scoped: true}, map[string]any{"agentId": agent.ID, "owner": owner, "repo": repo, "operations": operations, "ttlSeconds": command.TTL, "reason": command.Reason}, map[string]string{"Idempotency-Key": submission.Idempotency})
	if err != nil {
		return fmt.Errorf("%w; rerun the same command to resume safely", err)
	}
	if response.StatusCode == 401 || response.StatusCode == 409 {
		response.Body.Close()
		if err := transitionAgentSubmission(ctx, config, submission, nil); err != nil {
			return err
		}
		return errors.New("request capability expired or was already used; rerun the command to issue a fresh capability")
	}
	secrets := append(profileSecrets(config, ""), submission.Capability)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, err := decodeSessionResponse(response)
		return redactAgentError(err, secrets...)
	}
	record, err := apiRecord(response, []string{"id", "pollToken", "approvalUrl", "expiresAtUnix", "approvalExpiresAtUnix"}, "invalid API response")
	if err != nil {
		return err
	}
	for _, key := range []string{"id", "pollToken", "approvalUrl"} {
		if err := configField(record, key, "string", "", false); err != nil {
			return errors.New("invalid API response: invalid grant request")
		}
	}
	id, pollToken, approval := *record.stringValue("id"), *record.stringValue("pollToken"), *record.stringValue("approvalUrl")
	if !safeAPIIdentifier(id) || !cleanCredential(pollToken) || !validBrowserURL(approval) || privateJSON(map[string]any{"id": id, "url": approval}, append(secrets, pollToken)) {
		return errors.New("invalid API response: invalid grant request")
	}
	if run.now().Unix() < 0 {
		return errors.New("system clock is before Unix epoch")
	}
	fallback := uint64(run.now().Unix())
	fallback += min(command.TTL, math.MaxUint64-fallback)
	expires, err := optionalDeviceSeconds(record, "expiresAtUnix", fallback)
	if err != nil {
		return err
	}
	if err := configField(record, "approvalExpiresAtUnix", "uint", "null", true); err != nil {
		return errors.New("invalid API response: invalid approval expiry")
	}
	var approvalExpires *uint64
	_ = json.Unmarshal(record["approvalExpiresAtUnix"], &approvalExpires)
	pending := pendingAgentRequest{ID: id, AgentID: agent.ID, Owner: owner, Repo: repo, PollToken: pollToken, Operations: operations, Expires: expires, ApprovalExpires: approvalExpires}
	if err := transitionAgentSubmission(ctx, config, submission, &pending); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Human approval required:\n  request %s\n  %s\n", id, approval); err != nil {
		return err
	}
	if err := open(ctx, approval); err != nil {
		if _, err := fmt.Fprintln(stdout, "Unable to open a browser automatically."); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(stdout, "Waiting for a human to approve or deny..."); err != nil {
		return err
	}
	return pollAgentAccess(ctx, &id, stdout, stderr, run)
}

func matchingSubmission(config configuration, desired agentSubmission) (*agentSubmission, error) {
	var submissions []agentSubmission
	_ = json.Unmarshal(config["pending_agent_submissions"], &submissions)
	var selected *agentSubmission
	for _, candidate := range submissions {
		if candidate.AgentID == desired.AgentID && candidate.Owner == desired.Owner && candidate.Repo == desired.Repo && reflect.DeepEqual(candidate.Operations, desired.Operations) && candidate.TTL == desired.TTL && reflect.DeepEqual(candidate.Reason, desired.Reason) {
			if selected != nil {
				return nil, errors.New("multiple matching pending submissions; use a separate local co config per agent")
			}
			if !cleanCredential(candidate.Capability) || !cleanCredential(candidate.Idempotency) {
				return nil, errors.New("invalid pending submission")
			}
			copy := candidate
			selected = &copy
		}
	}
	return selected, nil
}

func prepareAgentSubmission(ctx context.Context, config configuration, desired agentSubmission) (agentSubmission, error) {
	if existing, err := matchingSubmission(config, desired); err != nil {
		return desired, err
	} else if existing != nil {
		return *existing, nil
	}
	credential, err := agentHumanCredential(config)
	if err != nil {
		return desired, err
	}
	response, err := requestAPI(ctx, effectiveAPI(config), "POST", "/agents/"+desired.AgentID+"/request-capabilities", credential, nil)
	if err != nil {
		return desired, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, err := decodeSessionResponse(response)
		return desired, redactAgentError(err, profileSecrets(config, "")...)
	}
	record, err := apiRecord(response, []string{"requestCapability"}, "invalid API response")
	if err != nil {
		return desired, err
	}
	if err := configField(record, "requestCapability", "string", "", false); err != nil || !cleanCredential(*record.stringValue("requestCapability")) {
		return desired, errors.New("invalid API response: invalid request capability")
	}
	desired.Capability = *record.stringValue("requestCapability")
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return desired, errors.New("unable to create request idempotency key")
	}
	desired.Idempotency = "co-" + hex.EncodeToString(key)
	err = mutateConfigContext(ctx, func(current configuration) error {
		if !sameConfigString(config, current, "api_url") || !sameConfigString(config, current, "session_token") {
			return errors.New("profile changed during access request; no capability saved")
		}
		if existing, err := matchingSubmission(current, desired); err != nil {
			return err
		} else if existing != nil {
			desired = *existing
			return nil
		}
		var records []json.RawMessage
		_ = json.Unmarshal(current["pending_agent_submissions"], &records)
		raw, _ := json.Marshal(desired)
		records = append(records, raw)
		current["pending_agent_submissions"], _ = json.Marshal(records)
		return nil
	})
	return desired, err
}

func transitionAgentSubmission(ctx context.Context, original configuration, submission agentSubmission, pending *pendingAgentRequest) error {
	return mutateConfigContext(ctx, func(current configuration) error {
		if !sameConfigString(original, current, "api_url") || !sameConfigString(original, current, "session_token") {
			return errors.New("profile changed during access request; no credentials saved")
		}
		var records []configuration
		_ = json.Unmarshal(current["pending_agent_submissions"], &records)
		retained := make([]configuration, 0, len(records))
		found := false
		for _, record := range records {
			var candidate agentSubmission
			raw, _ := json.Marshal(record)
			_ = json.Unmarshal(raw, &candidate)
			if candidate.Idempotency == submission.Idempotency {
				if found || !reflect.DeepEqual(candidate, submission) {
					return errors.New("pending submission changed; no credentials saved")
				}
				found = true
			} else {
				retained = append(retained, record)
			}
		}
		if !found {
			return errors.New("pending submission changed; no credentials saved")
		}
		if pending != nil {
			var requests []json.RawMessage
			_ = json.Unmarshal(current["pending_agent_requests"], &requests)
			for _, raw := range requests {
				var candidate pendingAgentRequest
				_ = json.Unmarshal(raw, &candidate)
				if candidate.ID == pending.ID {
					return errors.New("pending request already exists; run `co access wait` to resume")
				}
			}
			raw, _ := json.Marshal(pending)
			requests = append(requests, raw)
			current["pending_agent_requests"], _ = json.Marshal(requests)
		}
		current["pending_agent_submissions"], _ = json.Marshal(retained)
		return nil
	})
}
