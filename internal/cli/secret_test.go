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
	"time"
)

func TestCompiledSecretSetRustParity(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	for _, scenario := range []string{"owner", "repo", "stdin", "empty", "totp", "passkey", "no-method", "not-owner", "list-error", "challenge-error", "write-error", "private-error", "bad-receipt", "bad-name", "bad-scope", "both-scopes", "no-scope", "no-session", "oversize", "nul", "invalid-utf8", "help", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			profile := filepath.Join(root, "profile", "config.json")
			valuePath, proofPath := filepath.Join(root, "value"), filepath.Join(root, "proof")
			value := "  fixture secret\nsecond line\n"
			proof := "fixture password"
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
				if scenario == "list-error" && r.Method == "GET" || scenario == "challenge-error" && r.Method == "POST" || scenario == "write-error" && r.Method == "PUT" {
					w.WriteHeader(409)
					io.WriteString(w, `{"error":"version_conflict"}`)
					return
				}
				if scenario == "private-error" && r.Method == "PUT" {
					w.WriteHeader(500)
					json.NewEncoder(w).Encode(map[string]any{"error": value, "message": proof})
					return
				}
				switch r.Method {
				case "GET":
					methods := []string{"totp", "password", "passkey"}
					if scenario == "totp" {
						methods = []string{"totp", "passkey"}
					} else if scenario == "passkey" {
						methods = []string{"passkey"}
					} else if scenario == "no-method" {
						methods = []string{}
					}
					json.NewEncoder(w).Encode(map[string]any{"entries": []any{map[string]any{"kind": "secret", "name": "TOKEN", "version": 41}}, "canManageSecrets": scenario != "not-owner", "reauthenticationMethods": methods})
				case "POST":
					io.WriteString(w, `{"challengeId":"fixture-challenge"}`)
				case "PUT":
					if scenario == "bad-receipt" {
						io.WriteString(w, `{"name":"TOKEN","kind":"secret","version":41}`)
					} else {
						io.WriteString(w, `{"name":"TOKEN","kind":"secret","version":42}`)
					}
				}
			}))
			defer server.Close()
			config := fullConfig(server.URL)
			args := []string{"secret", "set", "TOKEN", "--owner", "owner", "--value-file", valuePath, "--reauth-file", proofPath}
			input := "stdin secret\n"
			switch scenario {
			case "repo":
				args[3], args[4] = "--repo", "owner/repo"
			case "stdin":
				args = []string{"secret", "set", "TOKEN", "--owner", "owner", "--reauth-file", proofPath}
			case "empty":
				value = ""
			case "bad-name":
				args[2] = "lowercase"
			case "bad-scope":
				args[4] = "UPPER"
			case "both-scopes":
				args = append(args, "--repo", "owner/repo")
			case "no-scope":
				args = args[:3]
			case "no-session":
				config["session_token"] = json.RawMessage(`null`)
			case "oversize":
				value = strings.Repeat("x", maxSecretBytes+1)
			case "nul":
				value = "nul\x00byte"
			case "invalid-utf8":
				value = string([]byte{255})
			case "help":
				args = []string{"secret", "set", "--help"}
			case "unknown":
				args = []string{"secret", "unknown"}
			}
			writeFixture(t, valuePath, value, 0600)
			writeFixture(t, proofPath, proof+"\r\n", 0600)
			writeConfigFixture(t, profile, config)
			before, _ := os.ReadFile(profile)
			var expected observation
			var expectedCalls []string
			for index, binary := range []string{rust, goBinary} {
				mu.Lock()
				calls = nil
				mu.Unlock()
				result := invokeInput(t, binary, root, args, []string{"CO_CONFIG_DIR=" + filepath.Dir(profile), "CO_API_URL=" + server.URL}, []byte(input))
				mu.Lock()
				requests := append([]string(nil), calls...)
				mu.Unlock()
				if index == 0 {
					expected, expectedCalls = result, requests
				} else if !reflect.DeepEqual(result, expected) || !reflect.DeepEqual(requests, expectedCalls) {
					t.Fatalf("secret parity differs in %s: Go=%+v Rust=%+v request-match=%v", scenario, result, expected, reflect.DeepEqual(requests, expectedCalls))
				}
				for _, secret := range []string{"fixture password", "fixture secret", "stdin secret", "fixture-human-session", "fixture-challenge"} {
					if strings.Contains(result.Stdout+result.Stderr, secret) {
						t.Fatal("secret or proof exposed")
					}
				}
				after, _ := os.ReadFile(profile)
				if string(before) != string(after) {
					t.Fatal("secret command changed local credentials")
				}
			}
		})
	}
	t.Log("23 compiled secret-set cases match Rust output, exact value/proof payloads, version and human authority; secret errors never reflect private input")
}

func TestSecretInputBoundariesAndHiddenEditing(t *testing.T) {
	for _, test := range []struct{ input, want string }{{"a\r", "a"}, {"日本語\x7fX\r", "日本X"}, {"ab\x08c\n", "ac"}, {"\r", ""}} {
		got, err := readHiddenLine(strings.NewReader(test.input), 100)
		if err != nil || got != test.want {
			t.Fatal(test, got, err)
		}
	}
	for _, value := range []string{"a\x03", "a\x04", strings.Repeat("x", 101) + "\r", string([]byte{255, '\r'})} {
		if _, err := readHiddenLine(strings.NewReader(value), 100); err == nil {
			t.Fatal("invalid hidden input accepted")
		}
	}
	for _, value := range []string{"", "  value\n", strings.Repeat("x", maxSecretBytes)} {
		if got, err := readBoundedSecret(context.Background(), strings.NewReader(value), maxSecretBytes); err != nil || got != value {
			t.Fatal("bounded reader changed input", err)
		}
	}
}

func TestSecretPipeCancellationDoesNotWaitForEOF(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := readSecretValue(ctx, reader, nil); done <- err }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal("secret pipe ignored cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("secret pipe waited for EOF after cancellation")
	}
}
