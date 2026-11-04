package cli

import (
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
	"sync/atomic"
	"testing"
)

func TestCompiledDeploymentRustParity(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	id := "12345678-abcd-ABCD-1234-123456789abc"
	for _, scenario := range []string{"list", "list-redacted", "configure", "set-secret", "environment-secret", "approve", "rollback", "totp", "passkey", "no-environment", "bad-receipt", "bad-approval", "list-error", "config-error", "challenge-error", "apply-error", "private-error", "no-session", "agent", "empty-agent", "bad-policy", "large-policy", "bad-environment", "bad-id", "duplicate", "help", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			profile := filepath.Join(root, "profile", "config.json")
			proofPath, policyPath, valuePath := filepath.Join(root, "proof"), filepath.Join(root, "policy"), filepath.Join(root, "value")
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if r.ContentLength > 0 {
					_ = json.NewDecoder(r.Body).Decode(&body)
				}
				raw, _ := json.Marshal(body)
				mu.Lock()
				calls = append(calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(raw))
				mu.Unlock()
				if scenario == "list-error" && strings.HasSuffix(r.URL.Path, "/deployments") || scenario == "config-error" && strings.HasSuffix(r.URL.Path, "/deployment-config") || scenario == "challenge-error" && strings.HasSuffix(r.URL.Path, "/reauthentication") || scenario == "apply-error" && strings.HasSuffix(r.URL.Path, "/apply") || scenario == "private-error" && r.Method == "POST" {
					w.WriteHeader(403)
					io.WriteString(w, `{"error":"fixture-deployment-password fixture-deployment-value fixture-human-session"}`)
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/deployments"):
					if scenario == "list-redacted" {
						io.WriteString(w, `{"token":"fixture-human-session","list":[]}`)
					} else {
						io.WriteString(w, `{"deployments":[{"id":"`+id+`","count":18446744073709551615,"name":"<日本語>"}]}`)
					}
				case strings.HasSuffix(r.URL.Path, "/deployment-config"):
					methods := []string{"totp", "password"}
					if scenario == "totp" {
						methods = []string{"totp"}
					} else if scenario == "passkey" {
						methods = []string{"passkey"}
					}
					json.NewEncoder(w).Encode(map[string]any{"reauthenticationMethods": methods, "environments": []any{map[string]any{"name": "staging", "id": "environment", "version": 7}}, "secrets": []any{map[string]any{"name": "TOKEN", "environmentId": nil, "version": 3}, map[string]any{"name": "TOKEN", "environmentId": "environment", "version": 4}}})
				case strings.HasSuffix(r.URL.Path, "/reauthentication"):
					io.WriteString(w, `{"challengeId":"deployment-challenge"}`)
				case strings.HasSuffix(r.URL.Path, "/approve"):
					json.NewEncoder(w).Encode(map[string]any{"id": id, "approved": scenario != "bad-approval"})
				case strings.HasSuffix(r.URL.Path, "/rollback"):
					io.WriteString(w, `{"id":"87654321-abcd-abcd-1234-123456789abc"}`)
				case strings.HasSuffix(r.URL.Path, "/apply"):
					operation, _ := body["operation"].(map[string]any)
					version := operation["version"].(float64) + 1
					if scenario == "bad-receipt" {
						version++
					}
					json.NewEncoder(w).Encode(map[string]any{"action": operation["action"], "environment": operation["environment"], "version": version})
				default:
					t.Error("unexpected deployment request")
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			config := fullConfig(server.URL)
			args := []string{"deployment", "configure", "owner/repo", "staging", "--file", policyPath, "--reauth-file", proofPath}
			policy := `{"runtime":"container","count":18446744073709551615,"metadata":{"b":true,"a":"日本語"}}`
			extraEnv := []string{}
			switch scenario {
			case "list", "list-redacted", "list-error":
				args = []string{"deployment", "list", "owner/repo"}
			case "set-secret", "environment-secret", "no-environment":
				args = []string{"deployment", "set-secret", "owner/repo", "TOKEN", "--value-file", valuePath, "--reauth-file", proofPath}
				if scenario != "set-secret" {
					environment := "staging"
					if scenario == "no-environment" {
						environment = "missing"
					}
					args = append(args, "--environment", environment)
				}
			case "approve", "bad-approval":
				args = []string{"deployment", "approve", "owner/repo", id, "--reauth-file", proofPath}
			case "rollback":
				args = []string{"deployment", "rollback", "owner/repo", id, "--reauth-file", proofPath}
			case "no-session":
				config["session_token"] = json.RawMessage(`null`)
			case "agent":
				extraEnv = append(extraEnv, "CO_AGENT_ID=agent")
			case "empty-agent":
				extraEnv = append(extraEnv, "CO_AGENT_ID=")
			case "bad-policy":
				policy = "{broken"
			case "large-policy":
				policy = strings.Repeat("x", 100001)
			case "bad-environment":
				args[3] = "UPPER"
			case "bad-id":
				args = []string{"deployment", "approve", "owner/repo", "bad"}
			case "duplicate":
				args = append(args, "--file", policyPath)
			case "help":
				args = []string{"deployment", "--help"}
			case "unknown":
				args = []string{"deployment", "unknown"}
			}
			writeFixture(t, policyPath, policy, 0600)
			writeFixture(t, proofPath, "fixture-deployment-password\n", 0600)
			writeFixture(t, valuePath, "fixture-deployment-value\n", 0600)
			writeConfigFixture(t, profile, config)
			before, _ := os.ReadFile(profile)
			var expected observation
			var expectedCalls []string
			for index, binary := range []string{rust, goBinary} {
				mu.Lock()
				calls = nil
				mu.Unlock()
				env := append([]string{"CO_CONFIG_DIR=" + filepath.Dir(profile), "CO_API_URL=" + server.URL}, extraEnv...)
				result := invoke(t, binary, root, args, env)
				mu.Lock()
				requests := append([]string(nil), calls...)
				mu.Unlock()
				if index == 0 {
					expected, expectedCalls = result, requests
				} else if !reflect.DeepEqual(result, expected) || !reflect.DeepEqual(requests, expectedCalls) {
					t.Fatalf("deployment parity differs in %s: Go=%+v Rust=%+v requests-match=%v", scenario, result, expected, reflect.DeepEqual(requests, expectedCalls))
				}
				for _, private := range []string{"fixture-deployment-password", "fixture-deployment-value", "fixture-human-session", "deployment-challenge"} {
					if strings.Contains(result.Stdout+result.Stderr, private) {
						t.Fatal("deployment exposed private input")
					}
				}
				after, _ := os.ReadFile(profile)
				if string(before) != string(after) {
					t.Fatal("deployment changed profile")
				}
			}
		})
	}
	t.Log("27 compiled deployment cases match Rust outputs, human authority, operations, version checks and reauthentication; fixture writes only")
}

func TestDeploymentRedirectsAndPrivateLists(t *testing.T) {
	for _, scenario := range []string{"redirect", "private-lineage", "cross-api"} {
		t.Run(scenario, func(t *testing.T) {
			path := isolatedConfig(t)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if scenario == "redirect" {
					w.Header().Set("Location", "/followed")
					w.WriteHeader(307)
				} else {
					io.WriteString(w, `{"unexpected":"fixture-lineage"}`)
				}
			}))
			defer server.Close()
			config := fullConfig(server.URL)
			if scenario == "cross-api" {
				config["api_url"] = json.RawMessage(`"https://other.invalid"`)
			}
			writeConfigFixture(t, path, config)
			t.Setenv("CO_API_URL", server.URL)
			var stdout strings.Builder
			err := manageDeployment(context.Background(), deploymentCommand{Action: "list", Owner: "owner", Repo: "repo"}, strings.NewReader(""), &stdout)
			if err == nil || stdout.Len() != 0 || strings.Contains(err.Error(), "fixture-lineage") || calls.Load() > 1 {
				t.Fatal("unsafe deployment reply accepted", err)
			}
			if scenario == "cross-api" && calls.Load() != 0 {
				t.Fatal("cross-API session transmitted")
			}
		})
	}
}
