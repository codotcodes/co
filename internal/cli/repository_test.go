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
	"sync/atomic"
	"testing"
)

func TestCompiledRepositoryRustParity(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	for _, scenario := range []string{"view", "anonymous", "view-defaults", "view-scalar", "view-error", "view-bad-repo", "create", "org-public", "dot-name", "json", "unconfirmed", "unconfirmed-json", "create-error", "no-session", "help", "conflicting", "empty", "bad-owner", "bad-name", "unknown", "extra"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "profile", "config.json")
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
				if scenario == "view-error" || scenario == "create-error" {
					w.WriteHeader(403)
					io.WriteString(w, `{"message":"repository denied"}`)
					return
				}
				if r.Method == "GET" {
					switch scenario {
					case "view-defaults":
						io.WriteString(w, `{}`)
					case "view-scalar":
						io.WriteString(w, `null`)
					default:
						io.WriteString(w, `{"owner":"returned","name":"renamed","visibility":"public","defaultBranch":"main"}`)
					}
					return
				}
				created := "true"
				if strings.HasPrefix(scenario, "unconfirmed") {
					created = "false"
				}
				io.WriteString(w, `{"owner":"owner","name":"repo","visibility":"private","created":`+created+`,"a":"<text> 日本語","number":18446744073709551615,"nested":{"c":true,"b":"b"}}`)
			}))
			defer server.Close()
			config := fullConfig(server.URL)
			args := []string{"repo", "create", "repo"}
			if strings.HasPrefix(scenario, "view") || scenario == "anonymous" {
				args = []string{"repo", "view", "owner/repo"}
			}
			switch scenario {
			case "anonymous", "no-session":
				config["session_token"] = json.RawMessage(`null`)
			case "view-bad-repo":
				args[2] = "owner/../repo"
			case "org-public":
				args = []string{"repo", "create", "owner/repo", "--public"}
			case "dot-name":
				args[2] = ".config"
			case "json", "unconfirmed-json":
				args = append(args, "--json")
			case "help":
				args = []string{"repo", "create", "--unknown", "--help"}
			case "conflicting":
				args = append(args, "--private", "--public")
			case "empty":
				args = args[:2]
			case "bad-owner":
				args[2] = "UPPER/repo"
			case "bad-name":
				args[2] = "foo..bar"
			case "unknown":
				args = append(args, "--unknown")
			case "extra":
				args = append(args, "another")
			}
			writeConfigFixture(t, path, config)
			before, _ := os.ReadFile(path)
			var expected observation
			var expectedCalls []string
			for index, binary := range []string{rust, goBinary} {
				mu.Lock()
				calls = nil
				mu.Unlock()
				result := invoke(t, binary, root, args, []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL})
				mu.Lock()
				requests := append([]string(nil), calls...)
				mu.Unlock()
				if index == 0 {
					expected, expectedCalls = result, requests
				} else if !reflect.DeepEqual(result, expected) || !reflect.DeepEqual(requests, expectedCalls) {
					t.Fatalf("repository parity differs: Go=%+v Rust=%+v requests=%v/%v", result, expected, requests, expectedCalls)
				}
				after, _ := os.ReadFile(path)
				if string(before) != string(after) || strings.Contains(result.Stdout+result.Stderr, "fixture-human-session") {
					t.Fatal("repository API changed or exposed credentials")
				}
			}
		})
	}
	t.Log("21 compiled repository view/create cases match Rust output, request payload and authority; JSON metadata and uint64 precision preserved")
}

func TestRepositoryCreationUncertainOutcomeIsNotRetried(t *testing.T) {
	path := isolatedConfig(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		connection.Close()
	}))
	defer server.Close()
	writeConfigFixture(t, path, fullConfig(server.URL))
	t.Setenv("CO_API_URL", server.URL)
	err := createRepository(context.Background(), createRepositoryCommand{Name: "repo", Visibility: "private"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "creation may have completed") || calls.Load() != 1 {
		t.Fatal("uncertain create was retried or misreported", err, calls.Load())
	}
}

func TestRepositoryPrivateAndInvalidResponsesFailClosed(t *testing.T) {
	for _, reply := range []string{
		`{"owner":"owner","name":"fixture-human-session","visibility":"private","created":true}`,
		`{"owner":"owner","name":"repo","visibility":"private","created":true,"extra":"fixture-lineage"}`,
		`{"owner":"owner","name":"repo","visibility":"private","created":"true"}`,
		`{"owner":"owner","name":"repo","visibility":"private"}`,
	} {
		t.Run(reply, func(t *testing.T) {
			path := isolatedConfig(t)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.WriteString(w, reply)
			}))
			defer server.Close()
			config := fullConfig(server.URL)
			writeConfigFixture(t, path, config)
			t.Setenv("CO_API_URL", server.URL)
			var stdout, stderr bytes.Buffer
			err := createRepository(context.Background(), createRepositoryCommand{Name: "repo", Visibility: "private", JSON: true}, &stdout, &stderr)
			if err == nil || stdout.Len() != 0 || strings.Contains(err.Error(), "fixture-human-session") || strings.Contains(err.Error(), "fixture-lineage") {
				t.Fatal("unsafe repository response rendered", err)
			}
			config["api_url"] = json.RawMessage(`"https://other.invalid"`)
			writeConfigFixture(t, path, config)
			if err := viewRepository(context.Background(), "owner/repo", &stdout); err == nil || calls.Load() != 1 {
				t.Fatal("repository command sent cross-API session", err)
			}
		})
	}
}
