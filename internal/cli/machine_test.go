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
	"testing"
)

func TestCompiledMachineEnrollmentRustParity(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	for _, scenario := range []string{"owner", "setup", "setup-first-line", "setup-public", "replace", "list", "heartbeat", "heartbeat-revoked", "heartbeat-retry", "heartbeat-denied", "heartbeat-missing", "heartbeat-other-api", "invalid-response", "api-error", "no-session", "invalid-key", "bad-owner", "bad-label", "both", "missing-label", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			profile := filepath.Join(root, "profile", "config.json")
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body any
				if r.ContentLength > 0 {
					_ = json.NewDecoder(r.Body).Decode(&body)
				}
				raw, _ := json.Marshal(body)
				mu.Lock()
				calls = append(calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(raw))
				mu.Unlock()
				switch scenario {
				case "heartbeat-revoked":
					w.WriteHeader(401)
				case "heartbeat-retry":
					w.WriteHeader(503)
				case "heartbeat-denied", "api-error":
					w.WriteHeader(403)
					io.WriteString(w, `{"error":"enrollment denied"}`)
					return
				}
				scope, credential := "owner", "co_runner_issued"
				if scenario == "setup-public" {
					scope = "public"
				} else if scenario == "invalid-response" {
					credential = "not-a-runner"
				}
				json.NewEncoder(w).Encode(map[string]any{"machineId": "new-machine", "scope": scope, "credential": credential})
			}))
			defer server.Close()
			config := fullConfig(server.URL)
			delete(config, "future")
			config["agents"] = json.RawMessage(`[{"id":"agent","name":"fixture"}]`)
			config["machines"], _ = json.Marshal([]machineIdentity{{ID: "machine", Scope: "owner", API: server.URL, Credential: "co_runner_fixture"}})
			args := []string{"machine", "enroll", "--owner", "owner", "--label", "fixture machine"}
			input := "co_setup_fixture\n"
			switch scenario {
			case "setup", "setup-first-line", "setup-public", "invalid-key":
				args = []string{"machine", "enroll", "--setup-key"}
				if scenario == "setup-first-line" {
					input = " co_setup_fixture \nignored remainder\n"
				} else if scenario == "invalid-key" {
					input = "not-a-setup-key\n"
				}
			case "replace":
				config["machines"], _ = json.Marshal([]machineIdentity{{ID: "new-machine", Scope: "owner", API: server.URL, Credential: "co_runner_previous"}})
			case "list":
				args = []string{"machine", "list"}
			case "heartbeat", "heartbeat-revoked", "heartbeat-retry", "heartbeat-denied", "heartbeat-missing", "heartbeat-other-api":
				args = []string{"machine", "heartbeat", "machine"}
				if scenario == "heartbeat-missing" {
					args[2] = "missing"
				} else if scenario == "heartbeat-other-api" {
					config["machines"], _ = json.Marshal([]machineIdentity{{ID: "machine", Scope: "owner", API: "https://other.invalid", Credential: "co_runner_fixture"}})
				}
			case "no-session":
				config["session_token"] = json.RawMessage(`null`)
			case "bad-owner":
				args[3] = "UPPER"
			case "bad-label":
				args[5] = "  "
			case "both":
				args = append(args, "--setup-key")
			case "missing-label":
				args = args[:4]
			case "duplicate":
				args = append(args, "--owner", "other")
			}
			var expected observation
			var expectedConfig configuration
			var expectedCalls []string
			for index, binary := range []string{rust, goBinary} {
				writeConfigFixture(t, profile, config)
				mu.Lock()
				calls = nil
				mu.Unlock()
				result := invokeInput(t, binary, root, args, []string{"CO_CONFIG_DIR=" + filepath.Dir(profile), "CO_API_URL=" + server.URL}, []byte(input))
				raw, _ := os.ReadFile(profile)
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
					t.Fatalf("machine parity differs: Go=%+v Rust=%+v config=%v requests=%v/%v", result, expected, reflect.DeepEqual(stored, expectedConfig), requests, expectedCalls)
				}
				for _, private := range []string{"co_setup_fixture", "co_runner_issued", "co_runner_fixture", "fixture-human-session"} {
					if strings.Contains(result.Stdout+result.Stderr, private) {
						t.Fatal("machine credential exposed")
					}
				}
			}
		})
	}
	t.Log("21 compiled machine enrollment/list/heartbeat cases match Rust outputs, host resources, exact authority and stored records")
}

func TestMachineEnrollmentMergeAndStaleProtection(t *testing.T) {
	path := isolatedConfig(t)
	writeConfigFixture(t, path, fullConfig("https://api.co.codes"))
	original, _ := loadConfig()
	machine := machineIdentity{ID: "new-machine", Scope: "owner", API: "https://api.co.codes", Credential: "co_runner_issued"}
	if err := mutateConfig(func(current configuration) error {
		current["future"] = json.RawMessage(`{"concurrent":true}`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := storeMachineEnrollment(context.Background(), original, machine, true); err != nil {
		t.Fatal(err)
	}
	stored, _ := loadConfig()
	if !strings.Contains(string(stored["future"]), "concurrent") || !strings.Contains(string(stored["agents"]), "retained") {
		t.Fatal("enrollment replaced concurrent records")
	}
	before, _ := os.ReadFile(path)
	machine.Credential = "co_runner_stale"
	if err := storeMachineEnrollment(context.Background(), original, machine, true); err == nil {
		t.Fatal("stale enrollment overwrote newer machine credential")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("stale enrollment changed profile")
	}
}
