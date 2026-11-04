package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestConfigMutationCancellationKeepsCommittedCredentials(t *testing.T) {
	path := isolatedConfig(t)
	writeConfigFixture(t, path, fullConfig("https://fixture.invalid"))
	before, _ := os.ReadFile(path)
	file, err := openConfigLock(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := lockConfig(file); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	called := false
	err = mutateConfigContext(ctx, func(configuration) error { called = true; return nil })
	cancel()
	unlockConfig(file)
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatal("config lock wait ignored cancellation", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	err = mutateConfigContext(ctx, func(config configuration) error {
		config["session_token"] = json.RawMessage(`"must-not-publish"`)
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled config publication succeeded", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("cancellation changed credentials")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp"))
	if len(files) != 0 {
		t.Fatal("canceled temporary credentials leaked")
	}
}

func TestRustAndGoConfigLocksInteroperate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cross-process runtime verification requires a Windows host")
	}
	rust, _ := comparisonBinaries(t)
	path := isolatedConfig(t)
	requested := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requested <- struct{}{}
		writer.WriteHeader(200)
	}))
	defer server.Close()
	config := fullConfig(server.URL)
	writeConfigFixture(t, path, config)
	before, _ := os.ReadFile(path)
	file, err := openConfigLock(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := lockConfig(file); err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			unlockConfig(file)
		}
	}()
	done := make(chan observation, 1)
	go func() {
		done <- invoke(t, rust, filepath.Dir(path), []string{"logout"}, []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL})
	}()
	select {
	case <-requested:
	case <-time.After(5 * time.Second):
		t.Fatal("Rust did not reach signout fixture")
	}
	select {
	case <-done:
		t.Fatal("Rust bypassed the held Go config lock")
	case <-time.After(100 * time.Millisecond):
	}
	current, _ := os.ReadFile(path)
	if !bytes.Equal(before, current) {
		t.Fatal("blocked Rust writer changed the config")
	}
	if err := unlockConfig(file); err != nil {
		t.Fatal(err)
	}
	released = true
	select {
	case result := <-done:
		if result.Code != 0 {
			t.Fatal(result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Rust did not release after config unlock")
	}
	loaded, err := loadConfig()
	if err != nil || loaded.stringValue("session_token") != nil {
		t.Fatal("Rust config replacement is unreadable by Go", err)
	}
}

func TestConfigTemporaryFilesArePrivateAndSnapshotsAtomic(t *testing.T) {
	path := isolatedConfig(t)
	if err := secureConfigDirectory(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	file, err := createConfigTemp(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("temporary credential file was not private from creation")
	}
	file.Close()
	writeConfigFixture(t, path, fullConfig("https://fixture.invalid"))
	stop, done := make(chan struct{}), make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			data, err := readPrivateConfig(path)
			if err != nil {
				done <- err
				return
			}
			if _, err := parseConfig(data); err != nil {
				done <- err
				return
			}
		}
	}()
	for range 30 {
		if err := mutateConfig(func(config configuration) error { config["snapshot"] = json.RawMessage(`"complete"`); return nil }); err != nil {
			close(stop)
			<-done
			t.Fatal(err)
		}
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatal("reader observed a partial or missing config", err)
	}
}
