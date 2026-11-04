package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAgentHelperCancellationStillQuits(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process signal fixture")
	}
	goBinary := goBinaryForTests(t)
	root := t.TempDir()
	path := filepath.Join(root, "profile", "config.json")
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	writeConfigFixture(t, path, fullConfig(server.URL))
	before, _ := os.ReadFile(path)
	command := exec.Command(goBinary, "git-credential", "get")
	command.Dir = root
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "CO_AGENT_ID=agent", "CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL}
	command.Stdin = strings.NewReader(agentGitInput)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("mint did not start")
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled helper succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("helper ignored cancellation")
	}
	if stdout.String() != "quit=true\n\n" || strings.Contains(stderr.String(), "fixture-lineage") || strings.Contains(stderr.String(), "fixture-human-session") {
		t.Fatal("canceled helper failed open or leaked credentials")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("canceled mint changed private credentials")
	}
}
