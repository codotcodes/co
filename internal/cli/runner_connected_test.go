package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func connectedRunnerFixtureJob(api string, cache bool) map[string]any {
	step := map[string]any{"name": "build", "run": "fixture", "actionEnvironment": map[string]string{}}
	job := map[string]any{"id": "job", "assignmentToken": "assignment", "checkoutUrl": api + "/owner/repo.git", "commitOid": strings.Repeat("a", 40), "steps": []any{step}, "vcpus": 1, "ramMiB": 1, "scope": "owner"}
	if cache {
		step["github"], step["cache"] = true, true
		job["cacheWrite"], job["repositoryId"] = true, "repository"
	}
	return job
}

func fixtureRunnerEngine(ctx context.Context, stderr io.Writer) *runnerEngine {
	ctx, cancel := context.WithCancel(ctx)
	return &runnerEngine{ctx: ctx, cancel: cancel, cpus: 1, memory: 320, stderr: synchronizeRunnerWriter(stderr), timings: defaultRunnerTimings}
}

func TestRunnerSchedulerSlotsAndResultFence(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux runner scheduler fixture")
	}
	for _, scenario := range []string{"ack", "conflict", "uncertain", "over-capacity", "invalid-checkout", "unsafe-id"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			prepareRunnerJobFixture(t, root, "cache")
			var requests, finishes atomic.Int64
			var api string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer co_runner_fixture" {
					t.Error("scheduler lost machine authority")
				}
				if strings.HasSuffix(r.URL.Path, "/next-job") {
					if requests.Add(1) > 1 {
						w.WriteHeader(204)
						return
					}
					job := connectedRunnerFixtureJob(api, true)
					switch scenario {
					case "over-capacity":
						job["vcpus"] = 2
					case "invalid-checkout":
						job["checkoutUrl"] = "https://foreign.invalid/repo.git"
					case "unsafe-id":
						job["id"] = "../job"
					}
					json.NewEncoder(w).Encode(map[string]any{"job": job})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/finish") {
					finishes.Add(1)
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["assignmentToken"] != "assignment" {
						t.Error("finish omitted assignment fence")
					}
					if scenario == "over-capacity" || scenario == "invalid-checkout" {
						if body["result"] != "failure" || len(body["steps"].([]any)) != 0 {
							t.Error("invalid assignment did not fail without execution")
						}
					}
					if scenario == "conflict" {
						w.WriteHeader(409)
					} else if scenario == "uncertain" {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						connection.Close()
					} else {
						w.WriteHeader(204)
					}
					return
				}
				if strings.HasSuffix(r.URL.Path, "/log-deltas") {
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					json.NewEncoder(w).Encode(map[string]any{"type": "ack", "hash": body["update"].(map[string]any)["hash"]})
					return
				}
				w.WriteHeader(204)
			}))
			defer server.Close()
			api = server.URL
			var stderr bytes.Buffer
			runner := fixtureRunnerEngine(context.Background(), &stderr)
			defer runner.close()
			machine := machineIdentity{ID: "machine", API: api, Credential: "co_runner_fixture"}
			err := runner.poll(machine)
			if scenario == "unsafe-id" {
				if err == nil || finishes.Load() != 0 {
					t.Fatal("unsafe result endpoint was contacted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "over-capacity" || scenario == "invalid-checkout" {
				if len(runner.workers) != 0 || finishes.Load() != 1 {
					t.Fatal("invalid assignment consumed a worker slot")
				}
				return
			}
			if err := runner.poll(machine); err != nil || requests.Load() != 1 {
				t.Fatal("occupied CPU slot allowed another assignment", err)
			}
			select {
			case <-runner.workers[0].done:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not finish")
			}
			if finishes.Load() != 1 {
				t.Fatal("result was replayed or omitted")
			}
			cacheRoot := filepath.Join(root, "cache")
			cacheFiles := 0
			_ = filepath.WalkDir(cacheRoot, func(path string, entry os.DirEntry, err error) error {
				if err == nil && !entry.IsDir() && strings.HasSuffix(path, ".cache") {
					cacheFiles++
				}
				return nil
			})
			if (cacheFiles == 1) != (scenario == "ack") {
				t.Fatal("cache publication escaped successful result acknowledgment", cacheFiles)
			}
			if err := runner.poll(machine); err != nil || requests.Load() != 2 || len(runner.workers) != 0 {
				t.Fatal("completed worker did not release its CPU slot", err)
			}
		})
	}
}

func TestRunnerEnrollmentLockProtectsCleanup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux enrollment lock fixture")
	}
	root := t.TempDir()
	prepareRunnerJobFixture(t, root, "normal")
	t.Setenv("CO_CONFIG_DIR", root)
	clean := filepath.Join(root, "cleanup")
	t.Setenv("CO_STEP_CLEAN", clean)
	first, err := prepareConnectedRunner(context.Background(), "machine", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := prepareConnectedRunner(context.Background(), "machine", io.Discard); err == nil {
		second.Close()
		t.Fatal("second runner acquired the enrollment lock")
	}
	data, _ := os.ReadFile(clean)
	if strings.Count(string(data), "io.co.codes.machine=machine") != 1 {
		t.Fatal("second runner cleaned up containers before acquiring the lock")
	}
	first.Close()
	second, err := prepareConnectedRunner(context.Background(), "machine", io.Discard)
	if err != nil {
		t.Fatal("enrollment lock was not released", err)
	}
	second.Close()
}

func TestCompiledConnectedRunnerRustParity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux compiled connected-runner fixture")
	}
	rust, goBinary := comparisonBinaries(t)
	root := t.TempDir()
	prepareRunnerJobFixture(t, root, "normal")
	// The compiled clients use their actual host-proportional memory budget.
	path := filepath.Join(root, "bin", "podman")
	data, _ := os.ReadFile(path)
	data = bytes.Replace(data, []byte("'--memory=256m','--memory-swap=256m','--cpus=1',"), nil, 1)
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0500); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "profile", "config.json")
	var api string
	var mu sync.Mutex
	issued := false
	var finishBody, capabilities map[string]any
	finished := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer co_runner_fixture" {
			t.Error("compiled runner substituted human credentials")
		}
		if strings.HasSuffix(r.URL.Path, "/next-job") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			capabilities = body
			if issued {
				mu.Unlock()
				w.WriteHeader(204)
				return
			}
			issued = true
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"job": connectedRunnerFixtureJob(api, false)})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/finish") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			finishBody = body
			mu.Unlock()
			w.WriteHeader(204)
			finished <- struct{}{}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/log-deltas") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"type": "ack", "hash": body["update"].(map[string]any)["hash"]})
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	api = server.URL
	config := fullConfig(api)
	config["machines"], _ = json.Marshal([]machineIdentity{{ID: "machine", Scope: "owner", API: api, Credential: "co_runner_fixture"}})
	writeConfigFixture(t, profile, config)
	var expectedFinish, expectedCapabilities map[string]any
	for index, binary := range []string{rust, goBinary} {
		mu.Lock()
		issued, finishBody, capabilities = false, nil, nil
		mu.Unlock()
		command := exec.Command(binary, "machine", "run", "machine")
		command.Dir = root
		command.Env = runnerEnvironment(map[string]string{"CO_CONFIG_DIR": filepath.Dir(profile), "CO_API_URL": api})
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		select {
		case <-finished:
		case <-time.After(8 * time.Second):
			command.Process.Kill()
			<-done
			t.Fatal("compiled runner did not finish fixture job", stderr.String())
		}
		_ = command.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			command.Process.Kill()
			<-done
			t.Fatal("compiled runner did not stop")
		}
		mu.Lock()
		if index == 0 {
			expectedFinish, expectedCapabilities = finishBody, capabilities
		} else if !reflect.DeepEqual(finishBody, expectedFinish) || !reflect.DeepEqual(capabilities, expectedCapabilities) {
			t.Fatal("compiled connected runner result/hash or capabilities differ")
		}
		mu.Unlock()
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Running job job on machine machine") {
			t.Fatal("compiled runner output changed")
		}
		for _, private := range []string{"co_runner_fixture", "fixture-human-session", "assignment"} {
			if strings.Contains(stdout.String()+stderr.String(), private) {
				t.Fatal("compiled runner exposed private credentials")
			}
		}
	}
	t.Log("compiled Rust/Go machine run matches capabilities, scoped authority and exact final result/hash through heartbeat, cleanup, assignment, execution and finish")
}
