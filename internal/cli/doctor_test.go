package cli

import (
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

func TestCompiledDoctorRustParity(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	for _, scenario := range []string{"healthy", "absent", "invalid-session", "health-error", "git-error", "missing-git"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "profile", "config.json")
			bin := filepath.Join(root, "bin")
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls = append(calls, r.URL.Path+" "+r.Header.Get("Authorization"))
				mu.Unlock()
				if scenario == "health-error" && r.URL.Path == "/health" {
					w.WriteHeader(503)
				} else if scenario == "invalid-session" && r.URL.Path != "/health" {
					w.WriteHeader(401)
				}
				io.WriteString(w, `{"ignored":"fixture-human-session"}`)
			}))
			defer server.Close()
			config := fullConfig(server.URL)
			if scenario == "absent" {
				config["session_token"] = json.RawMessage(`null`)
			}
			if scenario != "missing-git" {
				code := "0"
				if scenario == "git-error" {
					code = "7"
				}
				writeFixture(t, filepath.Join(bin, "git"), "#!/bin/sh\nprintf 'git output must not be shown\\n'\nexit "+code+"\n", 0500)
			}
			writeConfigFixture(t, path, config)
			before, _ := os.ReadFile(path)
			var expected observation
			var expectedCalls []string
			for index, binary := range []string{rust, goBinary} {
				mu.Lock()
				calls = nil
				mu.Unlock()
				result := invoke(t, binary, root, []string{"doctor"}, []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL, "PATH=" + bin})
				mu.Lock()
				requests := append([]string(nil), calls...)
				mu.Unlock()
				if index == 0 {
					expected, expectedCalls = result, requests
				} else if !reflect.DeepEqual(result, expected) || !reflect.DeepEqual(requests, expectedCalls) {
					t.Fatalf("doctor differs: Go=%+v Rust=%+v requests=%v/%v", result, expected, requests, expectedCalls)
				}
				after, _ := os.ReadFile(path)
				if string(before) != string(after) || strings.Contains(result.Stdout+result.Stderr, "fixture-human-session") || strings.Contains(result.Stdout, "must not be shown") {
					t.Fatal("doctor exposed private or child output")
				}
			}
		})
	}
	t.Log("6 compiled doctor cases match Rust output and exact unauthenticated-health/human-session request boundaries")
}
