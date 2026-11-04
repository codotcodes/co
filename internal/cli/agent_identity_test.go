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

func TestCompiledAgentIdentityRustParity(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	for _, scenario := range []string{"register", "trim", "unicode", "duplicate", "denied", "no-session", "empty", "long", "list-empty", "list", "list-offline"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "profile", "config.json")
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var input map[string]string
				_ = json.Unmarshal(body, &input)
				mu.Lock()
				calls = append(calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+input["name"])
				mu.Unlock()
				if scenario == "denied" {
					w.WriteHeader(403)
					io.WriteString(w, `{"error":"registration denied"}`)
					return
				}
				json.NewEncoder(w).Encode(map[string]string{"id": "new-agent", "name": input["name"]})
			}))
			defer server.Close()
			config := fullConfig(server.URL)
			delete(config, "future")
			config["agents"] = json.RawMessage(`[{"id":"agent","name":"fixture"}]`)
			args := []string{"agent", "register", "agent"}
			switch scenario {
			case "trim":
				args[2] = "  agent\t"
			case "unicode":
				args[2] = "日本語"
			case "duplicate":
				config["agents"] = json.RawMessage(`[{"id":"new-agent","name":"old name"}]`)
			case "no-session":
				config["session_token"] = json.RawMessage(`null`)
			case "empty":
				args[2] = " "
			case "long":
				args[2] = strings.Repeat("a", 81)
			case "list-empty":
				args = []string{"agent", "list"}
				config["agents"] = json.RawMessage(`[]`)
			case "list", "list-offline":
				args = []string{"agent", "list"}
				config["agents"] = json.RawMessage(`[{"id":"one","name":"shared"},{"id":"two","name":"shared"}]`)
				if scenario == "list-offline" {
					config["session_token"] = json.RawMessage(`null`)
				}
			}
			var expected observation
			var expectedConfig configuration
			var expectedCalls []string
			for index, binary := range []string{rust, goBinary} {
				writeConfigFixture(t, path, config)
				mu.Lock()
				calls = nil
				mu.Unlock()
				result := invoke(t, binary, root, args, []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL})
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				stored, err := parseConfig(data)
				if err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				requests := append([]string(nil), calls...)
				mu.Unlock()
				if index == 0 {
					expected, expectedConfig, expectedCalls = result, stored, requests
				} else if !reflect.DeepEqual(result, expected) || !reflect.DeepEqual(requests, expectedCalls) || !reflect.DeepEqual(stored, expectedConfig) {
					t.Fatalf("identity parity differs: Go=%+v Rust=%+v config=%v calls=%v/%v", result, expected, reflect.DeepEqual(stored, expectedConfig), requests, expectedCalls)
				}
				for _, request := range requests {
					if !strings.HasPrefix(request, "POST /agents Bearer fixture-human-session ") {
						t.Fatal("registration used incorrect authority", request)
					}
				}
			}
		})
	}
	t.Log("11 compiled registration/list cases match Rust outputs, requests and profile records")
}

func TestRememberAgentMergesAndRejectsChangedSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("CO_CONFIG_DIR", filepath.Dir(path))
	config := fullConfig("https://api.co.codes")
	writeConfigFixture(t, path, config)
	original, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := mutateConfig(func(current configuration) error {
		current["agents"] = json.RawMessage(`[{"id":"other","name":"other","unknown":42}]`)
		current["unknown"] = json.RawMessage(`{"preserved":true}`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := rememberAgent(context.Background(), original, agentIdentity{ID: "new", Name: "new"}); err != nil {
		t.Fatal(err)
	}
	stored, _ := loadConfig()
	var agents []configuration
	_ = json.Unmarshal(stored["agents"], &agents)
	var unknown map[string]bool
	_ = json.Unmarshal(stored["unknown"], &unknown)
	if len(agents) != 2 || string(agents[0]["unknown"]) != "42" || *agents[1].stringValue("id") != "new" || !unknown["preserved"] {
		t.Fatal("registration overwrote concurrent fields")
	}
	if err := mutateConfig(func(current configuration) error {
		current["session_token"] = json.RawMessage(`"new-session"`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := rememberAgent(context.Background(), original, agentIdentity{ID: "stale", Name: "stale"}); err == nil {
		t.Fatal("stale registration accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("stale registration changed profile")
	}
}

func TestAgentRegistrationBindingRedirectAndPrivateResponse(t *testing.T) {
	var calls int
	var mode string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch mode {
		case "redirect":
			w.Header().Set("Location", "/followed")
			w.WriteHeader(307)
		case "private":
			io.WriteString(w, `{"id":"agent","name":"fixture-human-session"}`)
		case "error":
			w.WriteHeader(403)
			io.WriteString(w, `{"error":"fixture-human-session"}`)
		default:
			io.WriteString(w, `{"id":"bad/id","name":"name"}`)
		}
	}))
	defer server.Close()
	config := fullConfig(server.URL)
	t.Setenv("CO_API_URL", server.URL)
	for _, scenario := range []string{"redirect", "private", "error", "invalid"} {
		mode = scenario
		before := calls
		_, err := registerAgentRemote(context.Background(), config, "name")
		if err == nil || strings.Contains(err.Error(), "fixture-human-session") || calls != before+1 {
			t.Fatal("registration boundary failed", scenario, err)
		}
	}
	config["api_url"] = json.RawMessage(`"https://api.co.codes"`)
	before := calls
	if _, err := registerAgentRemote(context.Background(), config, "name"); err == nil || calls != before {
		t.Fatal("cross-API session sent")
	}
}
