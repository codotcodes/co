package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const accessExpired = "access request expired; run `co access request` again"

type pendingAgentRequest struct {
	ID              string   `json:"id"`
	AgentID         string   `json:"agent_id"`
	Owner           string   `json:"owner"`
	Repo            string   `json:"repo"`
	PollToken       string   `json:"poll_token"`
	Operations      []string `json:"operations"`
	Expires         uint64   `json:"requested_expires_unix"`
	ApprovalExpires *uint64  `json:"approval_expires_unix"`
	API             *string  `json:"api_url,omitempty"`
}

type accessRuntime struct {
	wait func(context.Context, time.Duration) error
	now  func() time.Time
}

func waitAgentAccess(args []string, stdout, stderr io.Writer) error {
	if len(args) > 1 {
		return errors.New("usage: co access wait [request-id]")
	}
	var id *string
	if len(args) == 1 {
		id = &args[0]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return pollAgentAccess(ctx, id, stdout, stderr, accessRuntime{wait: waitForDevice, now: time.Now})
}

func selectPendingRequest(config configuration, id *string) (configuration, pendingAgentRequest, error) {
	var records []configuration
	if err := json.Unmarshal(config["pending_agent_requests"], &records); err != nil {
		return nil, pendingAgentRequest{}, errors.New("invalid pending agent requests")
	}
	var selected configuration
	if id != nil {
		for _, record := range records {
			if value := record.stringValue("id"); value != nil && *value == *id {
				if selected != nil {
					return nil, pendingAgentRequest{}, errors.New("ambiguous pending request ID")
				}
				selected = record
			}
		}
		if selected == nil {
			return nil, pendingAgentRequest{}, fmt.Errorf("no local pending request %q", *id)
		}
	} else {
		switch len(records) {
		case 0:
			return nil, pendingAgentRequest{}, errors.New("no pending agent access request")
		case 1:
			selected = records[0]
		default:
			return nil, pendingAgentRequest{}, errors.New("multiple pending requests; pass the request ID shown by `co access request`")
		}
	}
	var pending pendingAgentRequest
	raw, _ := json.Marshal(selected)
	if err := json.Unmarshal(raw, &pending); err != nil {
		return nil, pending, errors.New("invalid pending agent request")
	}
	if effectiveAPI(config) != profileAPI(config) || pending.API != nil && strings.TrimRight(*pending.API, "/") != effectiveAPI(config) {
		return nil, pending, errors.New("agent request API does not match this profile; use the matching CO_CONFIG_DIR and CO_API_URL")
	}
	if !safeAPIIdentifier(pending.ID) || !validAgentIdentity(agentIdentity{ID: pending.AgentID, Name: "agent"}) || !cleanCredential(pending.PollToken) {
		return nil, pending, errors.New("invalid pending agent request")
	}
	if _, _, err := repositorySpec(pending.Owner + "/" + pending.Repo); err != nil {
		return nil, pending, errors.New("invalid pending repository")
	}
	return selected, pending, nil
}

func safeAPIIdentifier(id string) bool {
	return cleanCredential(id) && id != "." && id != ".." && !strings.ContainsAny(id, "/\\?#%")
}

func pollAgentAccess(ctx context.Context, id *string, stdout, stderr io.Writer, run accessRuntime) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	snapshot, pending, err := selectPendingRequest(config, id)
	if err != nil {
		return err
	}
	secrets := profileSecrets(config, "")
	expired := func() bool { return run.now().Unix() < 0 || uint64(run.now().Unix()) >= pending.Expires }
	finish := func(grant *agentGrant) error {
		return finishAgentRequest(ctx, config, snapshot, pending, grant, run.now)
	}
	backoff := uint64(2)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if expired() {
			if err := finish(nil); err != nil {
				return err
			}
			return errors.New(accessExpired)
		}
		response, err := requestAPI(ctx, effectiveAPI(config), "GET", "/grant-requests/"+pending.ID, &apiCredential{scheme: "Poll", secret: pending.PollToken, scoped: true}, nil)
		delay := backoff
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, err := fmt.Fprintf(stderr, "co: warning: %s; retrying\n", err); err != nil {
				return err
			}
		} else if response.StatusCode == 429 || response.StatusCode >= 500 && response.StatusCode <= 599 {
			if value, ok := approvalRetryAfter(response.Header.Get("Retry-After"), run.now()); ok {
				delay = max(1, value)
			}
			response.Body.Close()
			if _, err := fmt.Fprintf(stderr, "co: warning: approval service returned %d %s; retrying\n", response.StatusCode, http.StatusText(response.StatusCode)); err != nil {
				return err
			}
		} else {
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				_, err := decodeSessionResponse(response)
				return redactAgentError(err, secrets...)
			}
			record, err := apiRecord(response, []string{"status", "grantId", "lineageToken"}, "invalid API response")
			if err != nil {
				return err
			}
			if err := configField(record, "status", "string", "", false); err != nil {
				return errors.New("invalid API response: invalid request status")
			}
			for _, key := range []string{"grantId", "lineageToken"} {
				if err := configField(record, key, "string", "null", true); err != nil {
					return errors.New("invalid API response: invalid grant")
				}
			}
			if expired() {
				if err := finish(nil); err != nil {
					return err
				}
				return errors.New(accessExpired)
			}
			switch status := *record.stringValue("status"); status {
			case "pending":
				if pending.ApprovalExpires != nil && uint64(run.now().Unix()) >= *pending.ApprovalExpires {
					if err := finish(nil); err != nil {
						return err
					}
					return errors.New(accessExpired)
				}
			case "approved":
				grantID, lineage := record.stringValue("grantId"), record.stringValue("lineageToken")
				if grantID == nil {
					return errors.New("approved response omitted grantId")
				}
				if lineage == nil {
					return errors.New("approved response omitted lineageToken")
				}
				if !safeAPIIdentifier(*grantID) || !cleanCredential(*lineage) {
					return errors.New("invalid API response: invalid approved grant")
				}
				grant := agentGrant{AgentID: pending.AgentID, Owner: pending.Owner, Repo: pending.Repo, GrantID: *grantID, LineageToken: *lineage, Operations: pending.Operations, Expires: pending.Expires}
				if err := finish(&grant); err != nil {
					return err
				}
				_, err = fmt.Fprintf(stdout, "Access approved for %s/%s.\nRun `co access view %s/%s`.\n", pending.Owner, pending.Repo, pending.Owner, pending.Repo)
				return err
			case "denied", "expired":
				if err := finish(nil); err != nil {
					return err
				}
				if status == "denied" {
					return errors.New("access request was denied by the human approver")
				}
				return errors.New(accessExpired)
			default:
				return redactAgentError(fmt.Errorf("unknown access request status %q", status), secrets...)
			}
		}
		remaining := pending.Expires - min(pending.Expires, uint64(max(0, run.now().Unix())))
		if err := run.wait(ctx, deviceSeconds(min(delay, remaining))); err != nil {
			return err
		}
		backoff = min(backoff*2, 15)
	}
}

func approvalRetryAfter(value string, now time.Time) (uint64, bool) {
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		return seconds, true
	}
	date, err := http.ParseTime(value)
	if err != nil || date.Before(now) {
		return 0, false
	}
	return uint64(date.Sub(now) / time.Second), true
}

func finishAgentRequest(ctx context.Context, original, snapshot configuration, pending pendingAgentRequest, grant *agentGrant, now func() time.Time) error {
	return mutateConfigContext(ctx, func(current configuration) error {
		if !sameConfigString(original, current, "api_url") {
			return errors.New("profile changed during approval; no credentials saved")
		}
		var records []configuration
		_ = json.Unmarshal(current["pending_agent_requests"], &records)
		kept := make([]configuration, 0, len(records))
		found := false
		for _, candidate := range records {
			if id := candidate.stringValue("id"); id != nil && *id == pending.ID {
				if found || !sameRecord(candidate, snapshot) {
					return errors.New("pending request changed during approval; no credentials saved")
				}
				found = true
			} else {
				kept = append(kept, candidate)
			}
		}
		if !found {
			return errors.New("pending request changed during approval; no credentials saved")
		}
		if grant != nil {
			if now().Unix() < 0 || uint64(now().Unix()) >= pending.Expires {
				return errors.New(accessExpired)
			}
			var grants []configuration
			_ = json.Unmarshal(current["agent_grants"], &grants)
			var previous []configuration
			_ = json.Unmarshal(original["agent_grants"], &previous)
			matching := func(records []configuration) []configuration {
				var selected []configuration
				for _, candidate := range records {
					if agent, owner, repo := candidate.stringValue("agent_id"), candidate.stringValue("owner"), candidate.stringValue("repo"); agent != nil && owner != nil && repo != nil && *agent == pending.AgentID && *owner == pending.Owner && *repo == pending.Repo {
						selected = append(selected, candidate)
					}
				}
				return selected
			}
			before, _ := json.Marshal(matching(previous))
			after, _ := json.Marshal(matching(grants))
			if !bytes.Equal(before, after) {
				return errors.New("agent grant changed during approval; no credentials saved")
			}
			retained := make([]configuration, 0, len(grants)+1)
			for _, candidate := range grants {
				var existing agentGrant
				raw, _ := json.Marshal(candidate)
				_ = json.Unmarshal(raw, &existing)
				if existing.AgentID != pending.AgentID || existing.Owner != pending.Owner || existing.Repo != pending.Repo {
					retained = append(retained, candidate)
				}
			}
			raw, _ := json.Marshal(grant)
			var record configuration
			_ = json.Unmarshal(raw, &record)
			// Profile binding remains compatible with the Rust grant shape.
			delete(record, "api_url")
			retained = append(retained, record)
			current["agent_grants"], _ = json.Marshal(retained)
		}
		current["pending_agent_requests"], _ = json.Marshal(kept)
		return nil
	})
}

func sameRecord(left, right configuration) bool {
	decode := func(record configuration) any {
		raw, _ := json.Marshal(record)
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		_ = decoder.Decode(&value)
		return value
	}
	return reflect.DeepEqual(decode(left), decode(right))
}
