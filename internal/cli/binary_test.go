package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var testCLI struct {
	once       sync.Once
	root, path string
	err        error
}

// Contract tests always exercise Go; a supplied frozen binary adds Rust coverage.
func contractBinaries(t *testing.T) map[string]string {
	t.Helper()
	binaries := map[string]string{"go": goBinaryForTests(t)}
	if rust := os.Getenv("CO_TEST_RUST_BINARY"); rust != "" {
		binaries["rust"] = rust
	}
	return binaries
}

func goBinaryForTests(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("CO_TEST_GO_BINARY"); path != "" {
		return path
	}
	testCLI.once.Do(func() {
		testCLI.root, testCLI.err = os.MkdirTemp("", "co-test-cli-")
		if testCLI.err != nil {
			return
		}
		testCLI.path = filepath.Join(testCLI.root, "co")
		if runtime.GOOS == "windows" {
			testCLI.path += ".exe"
		}
		command := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-o", testCLI.path, "./cmd/co")
		command.Dir = filepath.Join("..", "..")
		if data, err := command.CombinedOutput(); err != nil {
			testCLI.err = fmt.Errorf("build CLI test binary: %w\n%s", err, data)
		}
	})
	if testCLI.err != nil {
		t.Fatal(testCLI.err)
	}
	return testCLI.path
}

func TestMain(tests *testing.M) {
	code := tests.Run()
	if testCLI.root != "" {
		if err := os.RemoveAll(testCLI.root); err != nil {
			fmt.Fprintln(os.Stderr, "unable to remove CLI test build:", err)
			code = 1
		}
	}
	os.Exit(code)
}
