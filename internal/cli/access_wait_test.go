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

func TestCompiledAccessWaitRustParity(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	for _, scenario := range []string{"approved", "denied", "expired", "local-expired", "approval-expired", "missing-grant", "missing-lineage", "unknown", "api-error", "no-pending", "missing-id", "multiple", "explicit", "pending", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "profile", "config.json")
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls = append(calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
				count := len(calls)
				mu.Unlock()
				switch scenario {
				case "denied", "expired", "unknown":
					json.NewEncoder(w).Encode(map[string]string{"status": scenario})
				case "missing-grant":
					io.WriteString(w, `{"status":"approved","lineageToken":"new-lineage"}`)
				case "missing-lineage":
					io.WriteString(w, `{"status":"approved","grantId":"new-grant"}`)
				case "approval-expired":
					io.WriteString(w, `{"status":"pending"}`)
				case "api-error":
					w.WriteHeader(403)
					io.WriteString(w, `{"error":"request revoked"}`)
				default:
					if count == 1 && scenario == "pending" {
						io.WriteString(w, `{"status":"pending"}`)
					} else if count == 1 && scenario == "retry" {
						w.Header().Set("Retry-After", "1")
						w.WriteHeader(503)
					} else {
						io.WriteString(w, `{"status":"approved","grantId":"new-grant","lineageToken":"new-lineage"}`)
					}
				}
			}))
			defer server.Close()
			config := fullConfig(server.URL)
			delete(config, "future")
			config["agents"] = json.RawMessage(`[{"id":"agent","name":"fixture"}]`)
			var pending []configuration
			_ = json.Unmarshal(config["pending_agent_requests"], &pending)
			args := []string{"access", "wait"}
			switch scenario {
			case "local-expired":
				pending[0]["requested_expires_unix"] = json.RawMessage(`1`)
			case "approval-expired":
				pending[0]["approval_expires_unix"] = json.RawMessage(`1`)
			case "no-pending":
				pending = []configuration{}
			case "missing-id":
				args = append(args, "missing")
			case "multiple", "explicit":
				var other configuration
				raw, _ := json.Marshal(pending[0])
				_ = json.Unmarshal(raw, &other)
				other["id"] = json.RawMessage(`"second"`)
				pending = append(pending, other)
				if scenario == "explicit" {
					args = append(args, "request")
				}
			}
			config["pending_agent_requests"], _ = json.Marshal(pending)
			var expected observation
			var expectedConfig configuration
			var expectedCalls []string
			for index, binary := range []string{rust, goBinary} {
				writeConfigFixture(t, path, config)
				mu.Lock()
				calls = nil
				mu.Unlock()
				result := invoke(t, binary, root, args, []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL})
				raw, _ := os.ReadFile(path)
				stored, err := parseConfig(raw)
				if err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				requests := append([]string(nil), calls...)
				mu.Unlock()
				if index == 0 {
					expected, expectedConfig, expectedCalls = result, stored, requests
				} else if !reflect.DeepEqual(result, expected) || !reflect.DeepEqual(requests, expectedCalls) || !reflect.DeepEqual(stored, expectedConfig) {
					t.Fatalf("wait parity differs: Go=%+v Rust=%+v config=%v calls=%v/%v", result, expected, reflect.DeepEqual(stored, expectedConfig), requests, expectedCalls)
				}
				for _, request := range requests {
					if request != "GET /grant-requests/request Poll fixture-poll" {
						t.Fatal("approval poll used wrong authority", request)
					}
				}
				for _, secret := range []string{"new-lineage", "fixture-poll", "fixture-human-session"} {
					if strings.Contains(result.Stdout+result.Stderr, secret) {
						t.Fatal("approval poll leaked credentials")
					}
				}
			}
		})
	}
	t.Log("15 compiled approval-wait cases match Rust outputs, scoped requests and stored grants; actual pending/backoff and Retry-After exercised")
}

func TestAccessWaitDeadlineCancellationAndConcurrentRecords(t *testing.T) {
	for _, scenario := range []string{"late", "pending-expiry", "cancel", "changed-pending", "changed-api", "changed-grant", "merge", "empty-token", "redirect", "private-error"} {
		t.Run(scenario, func(t *testing.T) {
			path := isolatedConfig(t)
			now := time.Unix(1000, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch scenario {
				case "late":
					now = now.Add(10 * time.Second)
				case "pending-expiry":
					io.WriteString(w, `{"status":"pending"}`)
					return
				case "cancel":
					cancel()
				case "changed-pending", "changed-api", "changed-grant", "merge":
					if err := mutateConfig(func(current configuration) error {
						switch scenario {
						case "changed-api":
							current["api_url"] = json.RawMessage(`"https://other.invalid"`)
						case "changed-grant":
							current["agent_grants"] = json.RawMessage(`[]`)
						case "changed-pending":
							var records []configuration
							_ = json.Unmarshal(current["pending_agent_requests"], &records)
							records[0]["poll_token"] = json.RawMessage(`"new-poll"`)
							current["pending_agent_requests"], _ = json.Marshal(records)
						case "merge":
							current["other"] = json.RawMessage(`42`)
						}
						return nil
					}); err != nil {
						t.Error(err)
					}
				case "empty-token":
					io.WriteString(w, `{"status":"approved","grantId":"new-grant","lineageToken":""}`)
					return
				case "redirect":
					w.Header().Set("Location", "/followed")
					w.WriteHeader(307)
					return
				case "private-error":
					w.WriteHeader(403)
					io.WriteString(w, `{"error":"fixture-poll fixture-human-session"}`)
					return
				}
				io.WriteString(w, `{"status":"approved","grantId":"new-grant","lineageToken":"new-lineage"}`)
			}))
			defer server.Close()
			config := fullConfig(server.URL)
			var pending []configuration
			_ = json.Unmarshal(config["pending_agent_requests"], &pending)
			pending[0]["requested_expires_unix"] = json.RawMessage(`1002`)
			config["pending_agent_requests"], _ = json.Marshal(pending)
			writeConfigFixture(t, path, config)
			t.Setenv("CO_API_URL", server.URL)
			var stdout, stderr bytes.Buffer
			run := accessRuntime{now: func() time.Time { return now }, wait: func(ctx context.Context, delay time.Duration) error {
				now = now.Add(delay)
				return ctx.Err()
			}}
			err := pollAgentAccess(ctx, nil, &stdout, &stderr, run)
			stored, loadErr := loadConfig()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if calls != 1 {
				t.Fatal("unexpected poll retry", calls)
			}
			if scenario == "merge" {
				if err != nil || string(stored["other"]) != "42" || !strings.Contains(string(stored["agent_grants"]), "new-lineage") || !strings.Contains(string(stored["agents"]), "retained") {
					t.Fatal("approval failed to merge", err)
				}
			} else if err == nil || stdout.Len() != 0 || strings.Contains(string(stored["agent_grants"]), "new-lineage") {
				t.Fatal("unsafe grant publication", err)
			}
			if err != nil && (strings.Contains(err.Error()+stderr.String(), "fixture-poll") || strings.Contains(err.Error()+stderr.String(), "fixture-human-session")) {
				t.Fatal("private approval error exposed")
			}
		})
	}
}

func TestApprovalRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		value string
		want  uint64
		ok    bool
	}{{"10", 10, true}, {"0", 0, true}, {now.Add(12 * time.Second).Format(http.TimeFormat), 12, true}, {now.Add(-time.Second).Format(http.TimeFormat), 0, false}, {"invalid", 0, false}, {"18446744073709551615", ^uint64(0), true}} {
		if value, ok := approvalRetryAfter(test.value, now); value != test.want || ok != test.ok {
			t.Fatal(test, value, ok)
		}
	}
}
