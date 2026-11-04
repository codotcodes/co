package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type loginOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	ready  chan struct{}
	once   sync.Once
}

func (output *loginOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	n, err := output.buffer.Write(data)
	if strings.Contains(output.buffer.String(), "Waiting for approval...") {
		output.once.Do(func() { close(output.ready) })
	}
	return n, err
}
func (output *loginOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.String()
}

func TestCompiledDeviceCancellationPreservesCredentials(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process signal fixtures")
	}
	rust, goBinary := comparisonBinaries(t)
	for _, phase := range []string{"wait", "http", "config-lock"} {
		for index, binary := range []string{rust, goBinary} {
			t.Run(phase+"/"+[]string{"rust", "go"}[index], func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "profile", "config.json")
				tokenRequest, release := make(chan struct{}, 1), make(chan struct{})
				var api string
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if strings.HasSuffix(request.URL.Path, "/code") {
						interval := uint64(0)
						if phase == "wait" {
							interval = 60
						}
						json.NewEncoder(writer).Encode(deviceFixture(api, interval, 120))
						return
					}
					tokenRequest <- struct{}{}
					if phase == "http" {
						select {
						case <-request.Context().Done():
							return
						case <-release:
							return
						}
					}
					io.WriteString(writer, `{"access_token":"`+issuedDeviceToken+`"}`)
				}))
				defer server.Close()
				defer close(release)
				api = server.URL
				writeConfigFixture(t, path, fullConfig(api))
				before, _ := os.ReadFile(path)
				if phase == "config-lock" {
					file, err := openConfigLock(path + ".lock")
					if err != nil {
						t.Fatal(err)
					}
					defer file.Close()
					if err := lockConfig(file); err != nil {
						t.Fatal(err)
					}
					defer unlockConfig(file)
				}
				browser := filepath.Join(root, "browser")
				writeFixture(t, browser, "#!/bin/sh\nexit 0\n", 0500)
				command := exec.Command(binary, "login")
				command.Dir = root
				command.Env = []string{"HOME=" + root, "CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + api, "PATH=" + root, "BROWSER=" + browser}
				stdout := &loginOutput{ready: make(chan struct{})}
				var stderr bytes.Buffer
				command.Stdout, command.Stderr = stdout, &stderr
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				defer command.Process.Kill()
				done := make(chan error, 1)
				go func() { done <- command.Wait() }()
				select {
				case <-stdout.ready:
				case <-done:
					t.Fatal("login exited before cancellation", stdout.String(), stderr.String())
				case <-time.After(5 * time.Second):
					t.Fatal("login was not ready")
				}
				if phase != "wait" {
					select {
					case <-tokenRequest:
					case <-time.After(3 * time.Second):
						t.Fatal("token poll did not start")
					}
				}
				if err := command.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("canceled login succeeded")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("canceled login failed to stop")
				}
				after, _ := os.ReadFile(path)
				if !bytes.Equal(before, after) {
					t.Fatal("canceled login replaced private credentials")
				}
				files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp"))
				if len(files) != 0 {
					t.Fatal("canceled login left private temporary files")
				}
				if strings.Contains(stdout.String(), issuedDeviceToken) || strings.Contains(stdout.String(), privateDeviceCode) {
					t.Fatal("cancellation exposed private device credentials")
				}
			})
		}
	}
	t.Log("Rust and Go cancellation preserve credentials during waits, pending HTTP and held config locks; Go exits cooperatively")
}
