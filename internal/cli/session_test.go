package cli

import (
	"bytes"
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

func TestCompiledSessionRustParity(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	count := 0
	for _, test := range []struct {
		name, command, body string
		status              int
		session             bool
	}{
		{"identity", "whoami", `{"user":{"username":"fixture","name":"Fixture","email":"fixture@example.invalid"}}`, 200, true},
		{"name-fallback", "whoami", `{"user":{"name":"Fixture"}}`, 200, true},
		{"empty-username", "whoami", `{"user":{"username":"","name":"Fixture","email":""}}`, 200, true},
		{"null-user", "whoami", `{"user":null}`, 200, true},
		{"absent-user", "whoami", `{}`, 200, true},
		{"expired-session", "whoami", `{"error":"expired","message":"session refused","error_description":"fresh login required"}`, 401, true},
		{"message-error", "whoami", `{"message":"fixture denied"}`, 403, true},
		{"fallback-status", "whoami", `{"error":3}`, 503, true},
		{"no-session", "whoami", `{}`, 200, false},
		{"sign-out", "logout", `{}`, 200, true},
		{"already-revoked", "logout", `{}`, 401, true},
		{"revocation-warning", "logout", `{}`, 503, true},
		{"ignore-signout-body", "logout", `not JSON`, 202, true},
		{"local-only-sign-out", "logout", `{}`, 200, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "profile", "config.json")
			var mu sync.Mutex
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				mu.Lock()
				requests = append(requests, request.Method+" "+request.URL.Path+" "+request.Header.Get("Authorization")+" "+request.UserAgent())
				mu.Unlock()
				writer.WriteHeader(test.status)
				io.WriteString(writer, test.body)
			}))
			defer server.Close()
			config := fullConfig(server.URL + "///")
			delete(config, "future")
			config["agents"] = json.RawMessage(`[{"id":"agent","name":"fixture"}]`)
			if !test.session {
				config["session_token"] = json.RawMessage("null")
			}
			var expected observation
			var expectedConfig any
			for index, binary := range []string{rust, goBinary} {
				writeConfigFixture(t, path, config)
				actual := invoke(t, binary, root, []string{test.command}, []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL + "///", "CO_AGENT_ID=unrelated-agent"})
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var stored any
				if err := json.Unmarshal(data, &stored); err != nil {
					t.Fatal(err)
				}
				if index == 0 {
					expected, expectedConfig = actual, stored
				} else if !reflect.DeepEqual(actual, expected) || !reflect.DeepEqual(stored, expectedConfig) {
					t.Fatalf("session behavior differs: Go=%+v Rust=%+v", actual, expected)
				}
			}
			wantCalls := 0
			mu.Lock()
			defer mu.Unlock()
			if test.session {
				wantCalls = 2
			}
			if len(requests) != wantCalls {
				t.Fatalf("unexpected API calls: %d", len(requests))
			}
			if wantCalls != 0 {
				method, path := "GET", "/api/auth/get-session"
				if test.command == "logout" {
					method, path = "POST", "/api/auth/sign-out"
				}
				want := method + " " + path + " Bearer fixture-human-session co/" + Version
				for _, request := range requests {
					if request != want {
						t.Fatal("credential/API boundary changed")
					}
				}
			}
			count++
		})
	}
	if !t.Failed() {
		t.Logf("%d compiled session/logout cases match Rust outputs, requests and stored credential records", count)
	}
}

func TestSessionRedirectConfinementAgainstRustBaseline(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	root := t.TempDir()
	path := filepath.Join(root, "profile", "config.json")
	var mu sync.Mutex
	var observed []string
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		observed = append(observed, request.Header.Get("Authorization"))
		mu.Unlock()
		if request.URL.Path == "/first" {
			http.Redirect(writer, request, "/last", 302)
			return
		}
		io.WriteString(writer, `{"user":{"name":"Fixture"}}`)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/first", 302)
	}))
	defer origin.Close()
	writeConfigFixture(t, path, fullConfig(origin.URL))
	compare(t, rust, goBinary, root, []string{"whoami"}, []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + origin.URL})
	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 4 {
		t.Fatal("missing redirect observations")
	}
	for index, token := range observed[2:] {
		if token != "" {
			t.Fatalf("Go restored credentials at redirect hop %d", index)
		}
	}
	if observed[0] != "" {
		t.Fatal("Rust baseline did not strip the first cross-origin hop")
	}
	if observed[1] != "" {
		t.Log("retained Rust restores the token at the second redirected hop; Go deliberately retains the stripped credential state")
	}
}

func TestLogoutPreservesConcurrentNewSessionAndProfile(t *testing.T) {
	path := isolatedConfig(t)
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		<-release
		writer.WriteHeader(200)
	}))
	defer server.Close()
	t.Setenv("CO_API_URL", server.URL)
	writeConfigFixture(t, path, fullConfig(server.URL))
	done := make(chan error, 1)
	go func() { done <- logout(io.Discard, io.Discard) }()
	<-started
	err := mutateConfig(func(config configuration) error {
		config["session_token"] = json.RawMessage(`"new-fixture-session"`)
		config["concurrent"] = json.RawMessage("true")
		return nil
	})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	config, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if token := config.stringValue("session_token"); token == nil || *token != "new-fixture-session" || string(config["concurrent"]) != "true" {
		t.Fatal("logout overwrote a concurrent credential/profile update")
	}
}

func TestInvalidConfigAndResponseDoNotLeakSecrets(t *testing.T) {
	path := isolatedConfig(t)
	writeFixture(t, path, `{"session_token":{"secret":"fixture-secret"}}`, 0600)
	if err := whoami(io.Discard); err == nil || !strings.HasPrefix(err.Error(), "invalid config ") || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal("invalid config did not fail privately")
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, "broken fixture-secret JSON")
	}))
	defer server.Close()
	t.Setenv("CO_API_URL", server.URL)
	writeConfigFixture(t, path, fullConfig(server.URL))
	if err := whoami(io.Discard); err == nil || !strings.HasPrefix(err.Error(), "invalid API response:") || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal("invalid API response did not fail privately")
	}
	var output bytes.Buffer
	if err := Run([]string{"whoami", "extra"}, strings.NewReader(""), &output, io.Discard); err == nil {
		t.Fatal("malformed response accepted")
	}
}

func TestLogoutPreservesConcurrentAPIBinding(t *testing.T) {
	path := isolatedConfig(t)
	t.Setenv("CO_API_URL", "")
	os.Unsetenv("CO_API_URL")
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		<-release
		writer.WriteHeader(200)
	}))
	defer server.Close()
	writeConfigFixture(t, path, fullConfig(server.URL))
	done := make(chan error, 1)
	go func() { done <- logout(io.Discard, io.Discard) }()
	<-started
	err := mutateConfig(func(config configuration) error {
		config["api_url"] = json.RawMessage(`"https://new-account.example.invalid"`)
		return nil
	})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	config, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.stringValue("session_token") == nil || effectiveAPI(config) != "https://new-account.example.invalid" {
		t.Fatal("logout changed a concurrent API/session binding")
	}
}
