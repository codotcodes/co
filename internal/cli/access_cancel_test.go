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

func TestCompiledAccessCancellationRetainsRecovery(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process signal fixture")
	}
	rust, goBinary := comparisonBinaries(t)
	for _, phase := range []string{"submit", "poll"} {
		for index, binary := range []string{rust, goBinary} {
			t.Run(phase+"/"+[]string{"rust", "go"}[index], func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "profile", "config.json")
				started, release := make(chan struct{}), make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if phase == "submit" && (r.URL.Path != "/grant-requests" || r.Header.Get("Authorization") != "Agent-Request fixture-capability") || phase == "poll" && (r.URL.Path != "/grant-requests/request" || r.Header.Get("Authorization") != "Poll fixture-poll") {
						t.Error("cancellation fixture used unexpected authority")
					}
					close(started)
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}))
				defer server.Close()
				defer close(release)
				writeConfigFixture(t, path, fullConfig(server.URL))
				before, _ := os.ReadFile(path)
				args := []string{"access", "wait"}
				if phase == "submit" {
					args = []string{"access", "request", "--push", "--reason", "fixture", "owner/repo"}
				}
				command := exec.Command(binary, args...)
				command.Dir = root
				command.Env = []string{"HOME=" + root, "PATH=" + os.Getenv("PATH"), "CO_API_URL=" + server.URL, "CO_CONFIG_DIR=" + filepath.Dir(path)}
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
					t.Fatal("request did not start")
				}
				if err := command.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("canceled operation succeeded")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("operation ignored cancellation")
				}
				after, _ := os.ReadFile(path)
				if !bytes.Equal(before, after) || stdout.Len() != 0 || strings.Contains(stderr.String(), "fixture-capability") || strings.Contains(stderr.String(), "fixture-poll") || strings.Contains(stderr.String(), "fixture-human-session") {
					t.Fatal("canceled access operation lost recovery state or exposed credentials")
				}
			})
		}
	}
}
