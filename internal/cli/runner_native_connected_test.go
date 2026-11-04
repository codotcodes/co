package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCompiledRunnerNativeGitContainerArtifactLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("CO_TEST_REAL_PODMAN") != "1" {
		t.Skip("set CO_TEST_REAL_PODMAN=1 for full native runner acceptance")
	}
	binaryPath := goBinaryForTests(t)
	root := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	runGit := func(directory string, args ...string) string {
		t.Helper()
		command := exec.Command(git, args...)
		command.Dir = directory
		command.Env = runnerEnvironment(map[string]string{"HOME": root, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull, "GIT_AUTHOR_NAME": "fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_NAME": "fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid"})
		data, err := command.CombinedOutput()
		if err != nil {
			t.Fatal("native runner Git fixture setup failed", err, string(data))
		}
		return string(data)
	}
	seed := filepath.Join(root, "seed")
	if err := os.Mkdir(seed, 0700); err != nil {
		t.Fatal(err)
	}
	runGit(seed, "init", "--initial-branch=main")
	writeFixture(t, filepath.Join(seed, "input.txt"), "native input\n", 0600)
	runGit(seed, "add", "input.txt")
	runGit(seed, "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	oid := strings.TrimSpace(runGit(seed, "rev-parse", "HEAD"))
	serverRoot := filepath.Join(root, "server")
	bare := filepath.Join(serverRoot, "owner", "repo.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0700); err != nil {
		t.Fatal(err)
	}
	runGit(root, "clone", "--bare", seed, bare)
	runGit(bare, "update-server-info")
	jobID := "fixture-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	machineID := "fixture-native-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	var api string
	var mu sync.Mutex
	issued := false
	var artifact []byte
	var final map[string]any
	finished := make(chan struct{}, 1)
	files := http.FileServer(http.Dir(serverRoot))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/owner/") {
			if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("co:fixture-assignment")) {
				t.Error("native Git substituted checkout authority")
				w.WriteHeader(401)
				return
			}
			files.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer co_runner_native_fixture" {
			t.Error("native runner substituted machine authority")
			w.WriteHeader(401)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/next-job"):
			mu.Lock()
			if issued {
				mu.Unlock()
				w.WriteHeader(204)
				return
			}
			issued = true
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"job": map[string]any{"id": jobID, "assignmentToken": "fixture-assignment", "checkoutUrl": api + "/owner/repo.git", "commitOid": oid, "vcpus": 1, "ramMiB": 1, "scope": "owner", "steps": []any{map[string]any{"name": "build", "run": "cat input.txt\nmkdir dist\nprintf '<h1>native</h1>' > dist/index.html"}}, "artifact": map[string]string{"name": "site", "path": "dist"}}})
		case strings.HasSuffix(r.URL.Path, "/artifact"):
			data, _ := io.ReadAll(r.Body)
			if r.Header.Get("X-Co-Assignment") != "fixture-assignment" || r.Header.Get("X-Co-Artifact-Sha256") != runnerDigest(data) {
				t.Error("native artifact upload lost its assignment fence/digest")
			}
			mu.Lock()
			artifact = data
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"digest": runnerDigest(data), "size": len(data)})
		case strings.HasSuffix(r.URL.Path, "/log-deltas"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"type": "ack", "hash": body["update"].(map[string]any)["hash"]})
		case strings.HasSuffix(r.URL.Path, "/finish"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			final = body
			mu.Unlock()
			w.WriteHeader(204)
			w.(http.Flusher).Flush()
			finished <- struct{}{}
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	api = server.URL
	profile := filepath.Join(root, "profile", "config.json")
	writeConfigFixture(t, profile, configuration{"api_url": json.RawMessage(strconv.Quote(api)), "machines": json.RawMessage(`[{"id":"` + machineID + `","scope":"owner","api_url":"` + api + `","credential":"co_runner_native_fixture"}]`)})
	command := exec.Command(binaryPath, "machine", "run", machineID)
	command.Dir = root
	command.Env = runnerEnvironment(map[string]string{"CO_CONFIG_DIR": filepath.Dir(profile), "CO_API_URL": api, "CO_MACHINE_IMAGE": runnerDefaultImage, "TMPDIR": root})
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		command.Process.Kill()
		<-done
		t.Fatal("native compiled runner did not finish", stderr.String())
	}
	_ = command.Process.Signal(os.Interrupt)
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		command.Process.Kill()
		<-done
		t.Fatal("native compiled runner did not drain on cancellation")
	}
	mu.Lock()
	defer mu.Unlock()
	if final["result"] != "success" || final["assignmentToken"] != "fixture-assignment" || len(artifact) < 10 || string(artifact[:6]) != "COAR1\x00" {
		t.Fatal("native compiled lifecycle failed", final["result"])
	}
	manifestSize := int(binary.BigEndian.Uint32(artifact[6:10]))
	if manifestSize > len(artifact)-10 || string(artifact[10+manifestSize:]) != "<h1>native</h1>" {
		t.Fatal("native artifact payload changed")
	}
	steps := final["steps"].([]any)
	if len(steps) != 1 || steps[0].(map[string]any)["output"] != "native input\n" {
		t.Fatal("native compiled runner lost process output")
	}
	for _, private := range []string{"fixture-assignment", "co_runner_native_fixture"} {
		if strings.Contains(stdout.String()+stderr.String(), private) {
			t.Fatal("native compiled runner exposed authority")
		}
	}
	check := exec.Command("podman", "ps", "-a", "--filter", "name=co-"+jobID+"-0", "--format", "{{.ID}}")
	if data, err := check.Output(); err != nil || strings.TrimSpace(string(data)) != "" {
		t.Fatal("native compiled runner left its task-owned container behind", err)
	}
	t.Log("compiled Go machine run completes local controller heartbeat/assignment, native HTTP Git checkout, actual rootless container, artifact receipt and fenced success, then drains cleanly")
}
