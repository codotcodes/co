package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCompiledAccessRequestRustParity(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	for _, scenario := range []string{"approved", "denied", "push-reason", "local-id", "local-name", "remote-id", "remote-name", "register", "local-ambiguous", "remote-ambiguous", "multiple-local", "no-local", "no-session", "reuse", "capability-denied", "expired-capability", "conflict", "request-error", "bad-ttl", "ttl-low", "ttl-high", "missing-option", "unknown-option", "bad-repo", "long-reason"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "profile", "config.json")
			var api string
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body any
				if r.Body != nil && r.ContentLength != 0 {
					_ = json.NewDecoder(r.Body).Decode(&body)
				}
				raw, _ := json.Marshal(body)
				mu.Lock()
				calls = append(calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(raw))
				mu.Unlock()
				switch r.URL.Path {
				case "/agents":
					if r.Method == "POST" {
						io.WriteString(w, `{"id":"remote","name":"selector"}`)
					} else if scenario == "register" {
						io.WriteString(w, `{"agents":[]}`)
					} else if scenario == "remote-ambiguous" {
						io.WriteString(w, `{"agents":[{"id":"remote","name":"selector"},{"id":"other","name":"selector"}]}`)
					} else {
						io.WriteString(w, `{"agents":[{"id":"remote","name":"selector"}]}`)
					}
				case "/agents/agent/request-capabilities", "/agents/remote/request-capabilities":
					if scenario == "capability-denied" {
						w.WriteHeader(403)
						io.WriteString(w, `{"error":"capability denied"}`)
					} else {
						io.WriteString(w, `{"requestCapability":"issued-capability"}`)
					}
				case "/grant-requests":
					var stored configuration
					raw, err := os.ReadFile(path)
					if err == nil {
						stored, err = parseConfig(raw)
					}
					if err != nil || !strings.Contains(string(stored["pending_agent_submissions"]), r.Header.Get("Idempotency-Key")) || r.Header.Get("Idempotency-Key") == "" {
						t.Error("submission was not durable before mutation")
					}
					switch scenario {
					case "expired-capability":
						w.WriteHeader(401)
					case "conflict":
						w.WriteHeader(409)
					case "request-error":
						w.WriteHeader(403)
						io.WriteString(w, `{"error":"request denied"}`)
					default:
						json.NewEncoder(w).Encode(map[string]any{"id": "created", "pollToken": "issued-poll", "approvalUrl": api + "/approve", "expiresAtUnix": 4000000000, "approvalExpiresAtUnix": 4000000000})
					}
				case "/grant-requests/created":
					if scenario == "denied" {
						io.WriteString(w, `{"status":"denied"}`)
					} else {
						io.WriteString(w, `{"status":"approved","grantId":"issued-grant","lineageToken":"issued-lineage"}`)
					}
				default:
					t.Error("unexpected request", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			api = server.URL
			browser := filepath.Join(root, "bin", "browser")
			browserLog := filepath.Join(root, "browser.log")
			writeFixture(t, browser, "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$CO_BROWSER_LOG\"\n", 0500)
			config := fullConfig(api)
			delete(config, "future")
			config["agents"] = json.RawMessage(`[{"id":"agent","name":"fixture"}]`)
			args := []string{"access", "request", "owner/repo"}
			switch scenario {
			case "push-reason", "reuse":
				args = append(args, "--push", "--reason", "fixture")
			case "local-id":
				args = append(args, "--agent", "agent")
			case "local-name":
				args = append(args, "--agent", "fixture")
			case "remote-id":
				args = append(args, "--agent", "remote")
			case "remote-name", "register", "remote-ambiguous":
				args = append(args, "--agent", "selector")
			case "multiple-local", "local-ambiguous":
				config["agents"] = json.RawMessage(`[{"id":"agent","name":"fixture"},{"id":"other","name":"fixture"}]`)
				if scenario == "local-ambiguous" {
					args = append(args, "--agent", "fixture")
				}
			case "no-local":
				config["agents"] = json.RawMessage(`[]`)
			case "no-session":
				config["session_token"] = json.RawMessage(`null`)
			case "bad-ttl":
				args = append(args, "--ttl", "nan")
			case "ttl-low":
				args = append(args, "--ttl", "299")
			case "ttl-high":
				args = append(args, "--ttl", "2592001")
			case "missing-option":
				args = append(args, "--agent")
			case "unknown-option":
				args = append(args, "--unknown")
			case "bad-repo":
				args[2] = "owner/../repo"
			case "long-reason":
				args = append(args, "--reason", strings.Repeat("あ", 501))
			}
			if scenario == "push-reason" {
				config["pending_agent_submissions"] = json.RawMessage(`[]`)
			}
			var expected observation
			var expectedStored configuration
			var expectedCalls []string
			for index, binary := range []string{rust, goBinary} {
				writeConfigFixture(t, path, config)
				_ = os.Remove(browserLog)
				mu.Lock()
				calls = nil
				mu.Unlock()
				result := invoke(t, binary, root, args, []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + api, "CO_AGENT_ID=unrelated", "BROWSER=" + browser, "PATH=" + filepath.Dir(browser), "CO_BROWSER_LOG=" + browserLog, "XDG_DATA_HOME=" + root, "XDG_DATA_DIRS=" + root})
				raw, _ := os.ReadFile(path)
				stored, err := parseConfig(raw)
				if err != nil {
					t.Fatal(err)
				}
				// Generated idempotency keys differ; both remain durably attached to their exact payload.
				var submissions []configuration
				_ = json.Unmarshal(stored["pending_agent_submissions"], &submissions)
				for _, submission := range submissions {
					if *submission.stringValue("request_capability") == "issued-capability" {
						submission["idempotency_key"] = json.RawMessage(`"generated"`)
					}
				}
				stored["pending_agent_submissions"], _ = json.Marshal(submissions)
				mu.Lock()
				requests := append([]string(nil), calls...)
				mu.Unlock()
				if index == 0 {
					expected, expectedStored, expectedCalls = result, stored, requests
				} else if !reflect.DeepEqual(result, expected) || !reflect.DeepEqual(requests, expectedCalls) || !reflect.DeepEqual(stored, expectedStored) {
					t.Fatalf("request parity differs: Go=%+v Rust=%+v config=%v calls=%v/%v", result, expected, reflect.DeepEqual(stored, expectedStored), requests, expectedCalls)
				}
				for _, request := range requests {
					switch {
					case strings.HasPrefix(request, "POST /grant-requests "):
						if !strings.Contains(request, " Agent-Request ") {
							t.Fatal("submission authority changed")
						}
					case strings.HasPrefix(request, "GET /grant-requests/"):
						if !strings.Contains(request, " Poll issued-poll ") {
							t.Fatal("poll authority changed")
						}
					default:
						if !strings.Contains(request, " Bearer fixture-human-session ") {
							t.Fatal("identity authority changed")
						}
					}
				}
				if strings.Contains(result.Stdout, "Human approval required") {
					browserArgs, _ := os.ReadFile(browserLog)
					if string(browserArgs) != api+"/approve\n" {
						t.Fatal("approval browser handoff failed", string(browserArgs))
					}
				}
				for _, secret := range []string{"issued-capability", "issued-poll", "issued-lineage", "fixture-human-session"} {
					if strings.Contains(result.Stdout+result.Stderr, secret) {
						t.Fatal("request exposed credentials")
					}
				}
			}
		})
	}
	t.Log("25 compiled access request cases match Rust output, authority, payload, browser handoff and stored records, apart from generated idempotency key bytes")
}

func TestAccessRequestUncertainSubmissionReusesExactKeyAndPayload(t *testing.T) {
	path := isolatedConfig(t)
	var mu sync.Mutex
	var keys, bodies []string
	var api string
	capabilities := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/request-capabilities") {
			capabilities++
			io.WriteString(w, `{"requestCapability":"issued-capability"}`)
			return
		}
		if r.URL.Path == "/grant-requests" {
			raw, _ := io.ReadAll(r.Body)
			keys, bodies = append(keys, r.Header.Get("Idempotency-Key")), append(bodies, string(raw))
			if len(keys) == 1 {
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				connection.Close()
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "created", "pollToken": "issued-poll", "approvalUrl": api + "/approve", "expiresAtUnix": 4000000000})
			return
		}
		io.WriteString(w, `{"status":"approved","grantId":"issued-grant","lineageToken":"issued-lineage"}`)
	}))
	defer server.Close()
	api = server.URL
	config := fullConfig(api)
	writeConfigFixture(t, path, config)
	t.Setenv("CO_API_URL", api)
	command, _ := parseAccessRequest([]string{"owner/repo", "--reason", "uncertain"})
	var stdout, stderr bytes.Buffer
	run := accessRuntime{now: time.Now, wait: waitForDevice}
	open := func(context.Context, string) error { return nil }
	if err := submitAgentAccess(context.Background(), command, &stdout, &stderr, open, run); err == nil || !strings.Contains(err.Error(), "resume safely") {
		t.Fatal("uncertain submission did not retain recovery", err)
	}
	stored, _ := loadConfig()
	if !strings.Contains(string(stored["pending_agent_submissions"]), "issued-capability") {
		t.Fatal("uncertain submission discarded capability")
	}
	if err := submitAgentAccess(context.Background(), command, &stdout, &stderr, open, run); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || capabilities != 1 || keys[0] == "" || keys[0] != keys[1] || bodies[0] != bodies[1] {
		t.Fatal("uncertain write replay changed its identity", keys, capabilities)
	}
}

func TestAccessRequestRejectsUnsafeRepliesBeforeBrowserOrPolling(t *testing.T) {
	for _, scenario := range []string{"empty-capability", "capability-redirect", "request-redirect", "empty-poll", "bad-id", "file-url", "private-url", "bad-expiry", "private-error", "changed-session"} {
		t.Run(scenario, func(t *testing.T) {
			path := isolatedConfig(t)
			var api string
			calls, opened := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if strings.HasSuffix(r.URL.Path, "/request-capabilities") {
					switch scenario {
					case "empty-capability":
						io.WriteString(w, `{"requestCapability":""}`)
					case "capability-redirect":
						w.Header().Set("Location", "/redirected")
						w.WriteHeader(307)
					case "changed-session":
						if err := mutateConfig(func(current configuration) error {
							current["session_token"] = json.RawMessage(`"replacement-session"`)
							return nil
						}); err != nil {
							t.Error(err)
						}
						io.WriteString(w, `{"requestCapability":"issued-capability"}`)
					default:
						io.WriteString(w, `{"requestCapability":"issued-capability"}`)
					}
					return
				}
				if r.URL.Path != "/grant-requests" {
					t.Error("followed unsafe response", r.URL.Path)
					w.WriteHeader(403)
					return
				}
				if scenario == "request-redirect" {
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(307)
					return
				}
				if scenario == "private-error" {
					w.WriteHeader(403)
					io.WriteString(w, `{"error":"issued-capability fixture-human-session"}`)
					return
				}
				reply := map[string]any{"id": "created", "pollToken": "issued-poll", "approvalUrl": api + "/approve", "expiresAtUnix": 4000000000}
				switch scenario {
				case "empty-poll":
					reply["pollToken"] = ""
				case "bad-id":
					reply["id"] = "../other"
				case "file-url":
					reply["approvalUrl"] = "file:///private"
				case "private-url":
					reply["approvalUrl"] = api + "/approve?secret=issued-poll"
				case "bad-expiry":
					reply["expiresAtUnix"] = -1
				}
				json.NewEncoder(w).Encode(reply)
			}))
			defer server.Close()
			api = server.URL
			writeConfigFixture(t, path, fullConfig(api))
			t.Setenv("CO_API_URL", api)
			command, _ := parseAccessRequest([]string{"owner/repo"})
			var stdout, stderr bytes.Buffer
			err := submitAgentAccess(context.Background(), command, &stdout, &stderr, func(context.Context, string) error { opened++; return nil }, accessRuntime{now: time.Now, wait: waitForDevice})
			if err == nil || opened != 0 || stdout.Len() != 0 || calls > 2 {
				t.Fatal("unsafe submission accepted", err, calls, opened)
			}
			for _, secret := range []string{"issued-capability", "issued-poll", "fixture-human-session"} {
				if strings.Contains(err.Error()+stderr.String(), secret) {
					t.Fatal("private submission reply exposed")
				}
			}
		})
	}
}

func TestSubmissionTransitionPreservesConcurrentRecords(t *testing.T) {
	for _, scenario := range []string{"merge", "changed-submission", "changed-api", "existing-request"} {
		t.Run(scenario, func(t *testing.T) {
			path := isolatedConfig(t)
			writeConfigFixture(t, path, fullConfig("https://api.co.codes"))
			original, _ := loadConfig()
			var submissions []agentSubmission
			_ = json.Unmarshal(original["pending_agent_submissions"], &submissions)
			pending := pendingAgentRequest{ID: "created", AgentID: "agent", Owner: "owner", Repo: "repo", PollToken: "issued-poll", Operations: []string{"pull"}, Expires: 4000000000}
			if err := mutateConfig(func(current configuration) error {
				switch scenario {
				case "merge":
					current["future"] = json.RawMessage(`{"updated":true}`)
				case "changed-api":
					current["api_url"] = json.RawMessage(`"https://other.invalid"`)
				case "changed-submission":
					current["pending_agent_submissions"] = json.RawMessage(`[]`)
				case "existing-request":
					current["pending_agent_requests"], _ = json.Marshal([]pendingAgentRequest{pending})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			err := transitionAgentSubmission(context.Background(), original, submissions[0], &pending)
			after, _ := os.ReadFile(path)
			if scenario == "merge" {
				stored, _ := loadConfig()
				if err != nil || !strings.Contains(string(stored["future"]), "updated") || !strings.Contains(string(stored["pending_agent_requests"]), "issued-poll") {
					t.Fatal("concurrent records lost", err)
				}
			} else if err == nil || !bytes.Equal(before, after) {
				t.Fatal("stale submission changed config", err)
			}
		})
	}
}
