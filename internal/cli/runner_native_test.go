package cli

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunnerRealRootlessContainerIsolationAndCancellation(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("CO_TEST_REAL_PODMAN") != "1" {
		t.Skip("set CO_TEST_REAL_PODMAN=1 for actual rootless container acceptance")
	}
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	probe := exec.Command("podman", "info", "--format", "{{.Host.Security.Rootless}}")
	if data, err := probe.Output(); err != nil || strings.TrimSpace(string(data)) != "true" {
		t.Fatal("actual container acceptance requires rootless Podman", err)
	}
	writeFixture(t, filepath.Join(root, "input.txt"), "input fixture\n", 0600)
	environment := map[string]string{"TOKEN": "fixture-runner-secret", "CONTAINERS_CONF": "/not-a-host-config", "QUOTED": "a';touch /workspace/escaped;#\n$HOME"}
	keys := []string{"url", "version_id"}
	job := runnerAssignment{ID: "fixture-" + strconv.FormatInt(time.Now().UnixNano(), 10), MachineID: "fixture-task-owned", Token: "assignment", VCPUs: 1, RAMMiB: 256, Environment: environment, SecretValues: []string{environment["TOKEN"]}}
	step := runnerStep{Name: "native", Run: "test \"$CONTAINERS_CONF\" = /not-a-host-config\nprintf '%s' \"$QUOTED\" > quoted.txt\nprintf '%s\\n' \"$TOKEN\"\ncat input.txt\nprintf '{\"url\":\"ok\",\"version_id\":\"%s\"}' \"$TOKEN\" > \"$CO_ACTION_OUTPUT\"", ActionEnvironment: &environment, ActionOutputs: &keys}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	result := executeRunnerStep(ctx, runnerDefaultImage, root, step, job, &runnerLogs{}, "", "")
	cancel()
	if result.ExitCode != 0 || result.Output != "***\ninput fixture\n" || result.Outputs["url"] != "ok" || len(result.Outputs) != 1 {
		t.Fatalf("native container changed private output semantics: code=%d output=%q keys=%d", result.ExitCode, result.Output, len(result.Outputs))
	}
	if data, err := os.ReadFile(filepath.Join(root, "quoted.txt")); err != nil || string(data) != environment["QUOTED"] {
		t.Fatal("native environment quoting changed values", err)
	}
	if _, err := os.Stat(filepath.Join(root, "escaped")); !os.IsNotExist(err) {
		t.Fatal("environment value escaped its quoted assignment")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	result = executeRunnerStep(ctx, runnerDefaultImage, root, runnerStep{Name: "cancel", Run: "sleep 30"}, job, &runnerLogs{}, "", "")
	cancel()
	if result.ExitCode != 1 {
		t.Fatal("cancelled native container reported success")
	}
	check := exec.Command("podman", "ps", "-a", "--filter", "name=co-"+job.ID+"-0", "--format", "{{.ID}}")
	if data, err := check.Output(); err != nil || strings.TrimSpace(string(data)) != "" {
		t.Fatal("task-owned cancelled container was not removed", err)
	}
	t.Log("actual rootless pinned container preserves environment confinement/quoting, masked output, declared outputs and cancellation/removal")
}

func TestRunnerNativeGitCheckoutAndRedirectConfinement(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux native Git fixture")
	}
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
			t.Fatal("native Git fixture setup failed", args, err, string(data))
		}
		return string(data)
	}
	seed := filepath.Join(root, "seed")
	if err := os.Mkdir(seed, 0700); err != nil {
		t.Fatal(err)
	}
	runGit(seed, "init", "--initial-branch=main")
	writeFixture(t, filepath.Join(seed, "file"), "exact native commit\n", 0600)
	runGit(seed, "add", "file")
	runGit(seed, "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	oid := strings.TrimSpace(runGit(seed, "rev-parse", "HEAD"))
	serverRoot := filepath.Join(root, "server")
	bare := filepath.Join(serverRoot, "owner", "repo.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0700); err != nil {
		t.Fatal(err)
	}
	runGit(root, "clone", "--bare", seed, bare)
	runGit(bare, "update-server-info")
	var requests atomic.Int64
	files := http.FileServer(http.Dir(serverRoot))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("co:assignment")) {
			t.Error("native checkout omitted scoped Basic authority")
			w.WriteHeader(401)
			return
		}
		files.ServeHTTP(w, r)
	}))
	defer server.Close()
	checkout := filepath.Join(root, "checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	job := runnerAssignment{ID: "job", Token: "assignment", CheckoutURL: server.URL + "/owner/repo.git", CommitOID: oid, VCPUs: 1, RAMMiB: 256}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runnerCheckout(ctx, job, checkout, "http://api.co.localhost"); err != nil {
		t.Fatal(err)
	}
	if requests.Load() == 0 || strings.TrimSpace(runGit(filepath.Join(checkout, "source"), "rev-parse", "HEAD")) != oid {
		t.Fatal("native runner checkout lost exact commit identity")
	}
	if data, _ := os.ReadFile(filepath.Join(checkout, "source", "file")); string(data) != "exact native commit\n" {
		t.Fatal("native checkout lost content")
	}
	if data, _ := os.ReadFile(filepath.Join(checkout, "source", ".git", "config")); strings.Contains(string(data), "assignment") || strings.Contains(string(data), "Authorization") {
		t.Fatal("native checkout persisted assignment authority")
	}
	var foreign atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1); w.WriteHeader(403) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+r.URL.Path, 302) }))
	defer redirect.Close()
	job.CheckoutURL = redirect.URL + "/owner/repo.git"
	redirectRoot := filepath.Join(root, "redirect")
	if err := os.Mkdir(redirectRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := runnerCheckout(ctx, job, redirectRoot, "http://api.co.localhost"); err == nil || foreign.Load() != 0 {
		t.Fatal("native checkout followed a credential-bearing redirect", err)
	}
	t.Log("native Git fetches/checks out the exact local HTTP commit without persisting credentials and refuses cross-origin redirects")
}
