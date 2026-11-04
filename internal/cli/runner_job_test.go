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
	"sync/atomic"
	"testing"
	"time"
)

func prepareRunnerJobFixture(t *testing.T, root, scenario string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	git := `#!/usr/bin/env python3
import os,pathlib,sys
if sys.argv[1]=='clone':
 root=pathlib.Path('source');root.mkdir();(root/'dist').mkdir();(root/'dist'/'index.html').write_text('artifact fixture')
 assert os.environ['GIT_CONFIG_VALUE_0'].startswith('Authorization: Basic ')
sys.exit(0)
`
	writeFixture(t, filepath.Join(bin, "git"), strings.Replace(git, "#!/usr/bin/env python3", "#!"+python, 1), 0500)
	podman := strings.Replace(fixturePodman, "#!/usr/bin/env python3", "#!"+python, 1)
	podman = strings.Replace(podman, "pathlib.Path(os.environ['CO_STEP_CALLED']).write_text('called')", `
if os.environ['CO_STEP_CASE']=='cache':
 checkout=pathlib.Path(volumes['/workspace'][0]);(checkout/'deps').mkdir();(checkout/'deps'/'file').write_text('cache fixture')
 pathlib.Path(volumes['/run/co-job'][0],'cache-requests.json').write_text(json.dumps([{'key':'lock','path':'/workspace/deps'}]))
pathlib.Path(os.environ['CO_STEP_CALLED']).write_text('called')`, 1)
	writeFixture(t, filepath.Join(bin, "podman"), podman, 0500)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", root)
	t.Setenv("CO_STEP_CASE", scenario)
	t.Setenv("CO_STEP_CALLED", filepath.Join(root, "called"))
	t.Setenv("CO_STEP_STOP", filepath.Join(root, "stopped"))
	t.Setenv("CO_STEP_PRIVATE", "")
	t.Setenv("CO_MACHINE_IMAGE", "fixture@sha256:pinned")
	t.Setenv("CO_MACHINE_CACHE_DIR", filepath.Join(root, "cache"))
}

func TestRunnerJobMatchesCompiledRust(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux runner composition fixture")
	}
	binary := os.Getenv("CO_TEST_RUST_RUNNER_JOB_BINARY")
	if binary == "" {
		t.Skip("set the frozen retained Rust job probe")
	}
	for _, scenario := range []string{"normal", "failure", "artifact", "artifact-failure", "cache", "unpinned"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			prepareRunnerJobFixture(t, root, scenario)
			if scenario == "unpinned" {
				t.Setenv("CO_MACHINE_IMAGE", "unpinned")
			}
			var artifacts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer co_runner_fixture" {
					t.Error("runner composition used wrong machine authority")
				}
				if strings.HasSuffix(r.URL.Path, "/artifact") {
					artifacts.Add(1)
					data, _ := io.ReadAll(r.Body)
					if r.Header.Get("X-Co-Assignment") != "assignment" || r.Header.Get("X-Co-Artifact-Sha256") != runnerDigest(data) || r.Header.Get("Content-Type") != "application/vnd.co.artifact" {
						t.Error("artifact lost its assignment fence or digest")
					}
					if scenario == "artifact-failure" {
						w.WriteHeader(500)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"digest": runnerDigest(data), "size": len(data)})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/log-deltas") {
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					update := body["update"].(map[string]any)
					json.NewEncoder(w).Encode(map[string]any{"type": "ack", "hash": update["hash"]})
					return
				}
				w.WriteHeader(204)
			}))
			defer server.Close()
			step := map[string]any{"name": "build", "run": "fixture", "actionEnvironment": map[string]string{}}
			jobData := map[string]any{"id": "job", "assignmentToken": "assignment", "checkoutUrl": server.URL + "/owner/repo.git", "commitOid": strings.Repeat("a", 40), "steps": []any{step}, "vcpus": 1, "ramMiB": 256, "scope": "owner"}
			if scenario == "artifact" || scenario == "artifact-failure" {
				jobData["artifact"] = map[string]string{"name": "site", "path": "dist"}
			}
			if scenario == "cache" {
				step["github"], step["cache"] = true, true
				jobData["cacheWrite"], jobData["repositoryId"] = true, "repository"
			}
			input, _ := json.Marshal(map[string]any{"mode": "job", "job": jobData, "api": server.URL})
			command := exec.CommandContext(t.Context(), binary, "__migration-validate")
			command.Stdin = bytes.NewReader(append(input, '\n'))
			out, err := command.Output()
			if err != nil {
				t.Fatal("Rust job probe failed", err)
			}
			var expected struct {
				Steps        []runnerStepResult `json:"steps"`
				Success      bool               `json:"success"`
				Publications int                `json:"publications"`
			}
			if err := json.Unmarshal(out, &expected); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(jobData)
			job, err := decodeRunnerAssignment(raw)
			if err != nil {
				t.Fatal(err)
			}
			job.MachineID = "machine"
			steps, success, publications := executeRunnerJob(context.Background(), machineIdentity{ID: "machine", API: server.URL, Credential: "co_runner_fixture"}, job, time.Now(), defaultRunnerTimings, &runnerLogs{})
			if !reflect.DeepEqual(steps, expected.Steps) || success != expected.Success || len(publications) != expected.Publications {
				t.Fatalf("job differs: Go steps=%+v success=%v publications=%d Rust=%+v", steps, success, len(publications), expected)
			}
			if scenario == "artifact" || scenario == "artifact-failure" {
				if artifacts.Load() != 2 {
					t.Fatal("artifact was retried or omitted")
				}
			}
			if scenario == "cache" {
				entries, _ := os.ReadDir(publications[0].store.namespace)
				if len(entries) != 0 {
					t.Fatal("cache published before finish acknowledgment")
				}
			}
		})
	}
	t.Log("6 compiled retained-Rust/Go jobs match result steps, artifact outcomes and deferred cache publication; actual local HTTP receipts checked")
}

func TestRunnerLeaseCancellationAndRenewal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux runner lease fixture")
	}
	for _, scenario := range []string{"renewed", "revoked", "expired", "pending-renewal"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			prepareRunnerJobFixture(t, root, "cancel")
			if scenario == "renewed" {
				t.Setenv("CO_STEP_CASE", "normal")
			}
			var renewals atomic.Int64
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/renew") || r.Header.Get("Authorization") != "Bearer co_runner_fixture" {
					t.Error("lease renewal used wrong authority")
				}
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["assignmentToken"] != "assignment" {
					t.Error("lease renewal lost assignment fence")
				}
				renewals.Add(1)
				switch scenario {
				case "revoked":
					w.WriteHeader(409)
				case "expired":
					w.WriteHeader(500)
				case "pending-renewal":
					select {
					case <-r.Context().Done():
					case <-release:
					}
				default:
					w.WriteHeader(204)
				}
			}))
			defer server.Close()
			defer close(release)
			job := runnerAssignment{ID: "job", MachineID: "machine", Token: "assignment", CheckoutURL: server.URL + "/owner/repo.git", CommitOID: strings.Repeat("a", 40), VCPUs: 1, RAMMiB: 256, Steps: []runnerStep{{Name: "build", Run: "fixture"}}}
			timings := runnerTimings{lease: 500 * time.Millisecond, renewal: 30 * time.Millisecond, job: 3 * time.Second}
			_, success, publications := executeRunnerJob(context.Background(), machineIdentity{API: server.URL, Credential: "co_runner_fixture"}, job, time.Now(), timings, &runnerLogs{})
			if success != (scenario == "renewed") || len(publications) != 0 || renewals.Load() == 0 {
				t.Fatal("lease did not fence execution", success, renewals.Load())
			}
			before := renewals.Load()
			time.Sleep(80 * time.Millisecond)
			if renewals.Load() != before {
				t.Fatal("renewals continued after worker cleanup")
			}
		})
	}
}

func TestRunnerFinishDoesNotReplayUncertainWrites(t *testing.T) {
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
	err := finishRunnerJob(context.Background(), machineIdentity{API: server.URL, Credential: "co_runner_fixture"}, runnerAssignment{ID: "job", Token: "assignment"}, []runnerStepResult{{Name: "build", ExitCode: 0, Output: "ok"}}, true)
	if err == nil || calls.Load() != 1 {
		t.Fatal("uncertain result write was retried", err, calls.Load())
	}
}
