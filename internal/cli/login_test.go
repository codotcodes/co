package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const privateDeviceCode = "fixture-private-device-code"
const issuedDeviceToken = "fixture-issued-session"

func deviceFixture(api string, interval, expires uint64) map[string]any {
	return map[string]any{"device_code": privateDeviceCode, "user_code": "USER-CODE", "verification_uri": api + "/verify", "verification_uri_complete": api + "/verify?code=USER-CODE", "expires_in": expires, "interval": interval}
}

type deviceReply struct {
	status int
	body   string
}
type deviceRequest struct {
	method, path, auth, agent, contentType string
	body                                   map[string]any
}

func TestCompiledDeviceLoginRustParity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux compiled browser fixtures")
	}
	rust, goBinary := comparisonBinaries(t)
	for _, scenario := range []struct {
		name       string
		replies    []deviceReply
		expires    uint64
		noBrowser  bool
		codeStatus int
	}{
		{"approved", []deviceReply{{200, `{"access_token":"` + issuedDeviceToken + `"}`}}, 60, false, 200},
		{"pending", []deviceReply{{400, `{"error":"authorization_pending"}`}, {200, `{"access_token":"` + issuedDeviceToken + `"}`}}, 60, false, 200},
		{"slow-down", []deviceReply{{400, `{"error":"slow_down"}`}, {200, `{"access_token":"` + issuedDeviceToken + `"}`}}, 60, false, 200},
		{"denied", []deviceReply{{400, `{"error":"access_denied"}`}}, 60, false, 200},
		{"expired-token", []deviceReply{{400, `{"error":"expired_token"}`}}, 60, false, 200},
		{"expired-code", nil, 0, false, 200},
		{"manual-browser", []deviceReply{{400, `{"error":"access_denied"}`}}, 60, true, 200},
		{"api-error", nil, 60, false, 403},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "profile", "config.json")
			var mu sync.Mutex
			var calls []deviceRequest
			var times []time.Time
			poll := 0
			var api string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Error("invalid protocol request")
				}
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, deviceRequest{request.Method, request.URL.Path, request.Header.Get("Authorization"), request.UserAgent(), request.Header.Get("Content-Type"), body})
				if request.URL.Path == "/api/auth/device/code" {
					writer.WriteHeader(scenario.codeStatus)
					if scenario.codeStatus != 200 {
						io.WriteString(writer, `{"message":"fixture authorization refused"}`)
						return
					}
					json.NewEncoder(writer).Encode(deviceFixture(api, 0, scenario.expires))
					return
				}
				times = append(times, time.Now())
				if poll >= len(scenario.replies) {
					writer.WriteHeader(400)
					io.WriteString(writer, `{"error":"unexpected_poll"}`)
					return
				}
				reply := scenario.replies[poll]
				poll++
				writer.WriteHeader(reply.status)
				io.WriteString(writer, reply.body)
			}))
			defer server.Close()
			api = server.URL
			browser := filepath.Join(root, "bin", "fixture-browser")
			log := filepath.Join(root, "browser.log")
			writeFixture(t, browser, "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$CO_BROWSER_LOG\"\n", 0500)
			env := []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + api, "BROWSER=" + browser, "CO_BROWSER_LOG=" + log, "PATH=" + filepath.Dir(browser), "XDG_DATA_HOME=" + filepath.Join(root, "data"), "XDG_DATA_DIRS=" + filepath.Join(root, "data"), "CO_AGENT_ID=unrelated-agent"}
			if scenario.noBrowser {
				env = append(env, "BROWSER=not-present")
			}
			wantBrowser := api + "/verify?code=USER-CODE\n"
			if scenario.name == "approved" {
				env = append(env, "BROWSER="+browser+" --url=%s%% --colon=%c")
				wantBrowser = "--url=" + api + "/verify?code=USER-CODE%\n--colon=:\n"
			}
			initial := fullConfig(api)
			delete(initial, "future")
			initial["agents"] = json.RawMessage(`[{"id":"agent","name":"fixture"}]`)
			var expected observation
			var expectedConfig any
			var expectedCalls []deviceRequest
			for index, binary := range []string{rust, goBinary} {
				writeConfigFixture(t, path, initial)
				mu.Lock()
				calls = nil
				times = nil
				poll = 0
				mu.Unlock()
				_ = os.Remove(log)
				actual := invoke(t, binary, root, []string{"login"}, env)
				for _, secret := range []string{privateDeviceCode, issuedDeviceToken, "fixture-human-session"} {
					if strings.Contains(actual.Stdout+actual.Stderr, secret) {
						t.Fatal("login printed private credential material")
					}
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var stored any
				if err := json.Unmarshal(data, &stored); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				requests := append([]deviceRequest(nil), calls...)
				timestamps := append([]time.Time(nil), times...)
				mu.Unlock()
				if scenario.name == "slow-down" && len(timestamps) == 2 && timestamps[1].Sub(timestamps[0]) < 5900*time.Millisecond {
					t.Fatal("slow_down did not add five seconds")
				}
				if index == 0 {
					expected, expectedConfig, expectedCalls = actual, stored, requests
				} else if !reflect.DeepEqual(actual, expected) || !reflect.DeepEqual(stored, expectedConfig) || !reflect.DeepEqual(requests, expectedCalls) {
					t.Fatalf("login differs: Go=%+v Rust=%+v", actual, expected)
				}
				for _, request := range requests {
					if request.method != "POST" || request.auth != "" || request.agent != "co/"+Version || request.contentType != "application/json" {
						t.Fatal("device flow used account credentials or changed protocol metadata")
					}
					if request.body["client_id"] != "co-cli" {
						t.Fatal("wrong device client")
					}
					if request.path == "/api/auth/device/code" {
						if request.body["scope"] != "openid profile email" {
							t.Fatal("wrong scopes")
						}
					} else if request.path != "/api/auth/device/token" || request.body["device_code"] != privateDeviceCode || request.body["grant_type"] != deviceGrant {
						t.Fatal("wrong device polling payload")
					}
				}
				if !scenario.noBrowser && scenario.codeStatus == 200 {
					var opened []byte
					deadline := time.Now().Add(time.Second)
					for time.Now().Before(deadline) {
						opened, _ = os.ReadFile(log)
						if len(opened) > 0 {
							break
						}
						time.Sleep(5 * time.Millisecond)
					}
					if string(opened) != wantBrowser {
						t.Fatal("browser did not receive complete verification URI")
					}
				}
			}
		})
	}
	t.Log("8 compiled device flows match Rust output, requests, polling outcomes, browser handoff and persisted credential records")
}

func TestDeviceLoginDeadlineDefaultsAndConcurrentProfile(t *testing.T) {
	for _, mode := range []string{"default-interval", "expiry", "concurrent-session", "concurrent-grants", "empty-token", "api-binding", "late-token"} {
		t.Run(mode, func(t *testing.T) {
			path := isolatedConfig(t)
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			var waits []time.Duration
			var polls atomic.Int64
			var api string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if strings.HasSuffix(request.URL.Path, "/code") {
					code := deviceFixture(api, 60, 1)
					if mode != "expiry" {
						delete(code, "interval")
						delete(code, "expires_in")
					}
					json.NewEncoder(writer).Encode(code)
					return
				}
				polls.Add(1)
				if mode == "late-token" {
					clock.Add(int64(time.Hour))
				}
				if mode == "concurrent-session" {
					if err := mutateConfig(func(config configuration) error {
						config["session_token"] = json.RawMessage(`"newer-session"`)
						return nil
					}); err != nil {
						t.Error(err)
					}
				}
				if mode == "concurrent-grants" {
					if err := mutateConfig(func(config configuration) error { config["concurrent"] = json.RawMessage("true"); return nil }); err != nil {
						t.Error(err)
					}
				}
				if mode == "empty-token" {
					io.WriteString(writer, `{"access_token":""}`)
				} else {
					io.WriteString(writer, `{"access_token":"`+issuedDeviceToken+`"}`)
				}
			}))
			defer server.Close()
			api = server.URL
			t.Setenv("CO_API_URL", api)
			storedAPI := api
			if mode == "api-binding" {
				storedAPI = "https://previous.example.invalid"
			}
			initial := fullConfig(storedAPI)
			if mode == "api-binding" {
				initial, _ = parseConfig([]byte(`{"api_url":"https://previous.example.invalid","session_token":"fixture-human-session"}`))
			}
			writeConfigFixture(t, path, initial)
			run := loginRuntime{now: func() time.Time { return time.Unix(0, clock.Load()) }, open: func(context.Context, string) error { return nil }, wait: func(ctx context.Context, duration time.Duration) error {
				waits = append(waits, duration)
				clock.Add(int64(duration))
				return nil
			}}
			err := deviceLogin(t.Context(), io.Discard, run)
			config, loadErr := loadConfig()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			switch mode {
			case "expiry":
				if err == nil || err.Error() != deviceExpired || polls.Load() != 0 || len(waits) != 1 || waits[0] != time.Second {
					t.Fatal("device expiry was not enforced before a late poll", err)
				}
			case "concurrent-session":
				if err == nil || !strings.Contains(err.Error(), "profile changed") || *config.stringValue("session_token") != "newer-session" {
					t.Fatal("newer credentials overwritten", err)
				}
			case "empty-token":
				if err == nil || *config.stringValue("session_token") != "fixture-human-session" {
					t.Fatal("empty token replaced valid session")
				}
			case "late-token":
				if err == nil || err.Error() != deviceExpired || *config.stringValue("session_token") != "fixture-human-session" {
					t.Fatal("late token replaced credentials", err)
				}
			default:
				if err != nil || len(waits) != 1 || waits[0] != 5*time.Second || *config.stringValue("session_token") != issuedDeviceToken {
					t.Fatal("defaults or token persistence differ", err)
				}
				if apiValue := config.stringValue("api_url"); apiValue == nil || *apiValue != api {
					t.Fatal("session was not bound to the selected API")
				}
			}
			if mode == "concurrent-grants" && string(config["concurrent"]) != "true" {
				t.Fatal("concurrent profile update was discarded")
			}
		})
	}
}

func TestDeviceWaitCancellationAndDurationOverflow(t *testing.T) {
	isolatedConfig(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitForDevice(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if deviceSeconds(^uint64(0)) <= 0 {
		t.Fatal("device duration overflowed")
	}
	var output bytes.Buffer
	if err := deviceLogin(ctx, &output, loginRuntime{now: time.Now, wait: waitForDevice, open: func(context.Context, string) error { return nil }}); err == nil {
		t.Fatal("canceled login succeeded")
	}
}

func TestDeviceLoginRefusesForeignCredentialProfile(t *testing.T) {
	path := isolatedConfig(t)
	writeConfigFixture(t, path, fullConfig("https://previous.example.invalid"))
	before, _ := os.ReadFile(path)
	t.Setenv("CO_API_URL", "http://127.0.0.1:1")
	err := deviceLogin(t.Context(), io.Discard, loginRuntime{now: time.Now, wait: waitForDevice, open: func(context.Context, string) error {
		t.Fatal("foreign credential profile opened a browser")
		return nil
	}})
	if err == nil || !strings.Contains(err.Error(), "separate CO_CONFIG_DIR") {
		t.Fatal("foreign credentials were rebound", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("foreign profile was modified")
	}
}
