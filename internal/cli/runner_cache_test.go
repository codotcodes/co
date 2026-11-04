package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunnerCacheMatchesCompiledRust(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix cache collection")
	}
	binary := os.Getenv("CO_TEST_RUST_RUNNER_CACHE_BINARY")
	if binary == "" {
		t.Skip("set the frozen retained Rust cache probe")
	}
	for _, scenario := range []string{"workspace", "home", "unicode", "executable", "no-requests", "host-path", "traversal", "git-path", "symlink", "hardlink", "empty", "empty-key", "long-key", "too-many", "unmarked-root", "bad-marker"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			checkout, runtimeRoot := filepath.Join(root, "checkout"), filepath.Join(root, "runtime")
			requests := []runnerCacheRequest{{Key: "lock-key", Path: "/workspace/dependencies"}}
			writeFixture(t, filepath.Join(checkout, "dependencies", "@scope", "file"), "cached", 0600)
			writeFixture(t, filepath.Join(runtimeRoot, "home", "dependencies", "file"), "home cache", 0600)
			switch scenario {
			case "home":
				requests[0].Path = "/run/co-job/home/dependencies"
			case "unicode":
				writeFixture(t, filepath.Join(checkout, "dependencies", "日本語 file\u2028"), "UTF8", 0600)
			case "executable":
				writeFixture(t, filepath.Join(checkout, "dependencies", "executable"), "script", 0700)
			case "host-path":
				requests[0].Path = "/etc"
			case "traversal":
				requests[0].Path = "/workspace/../dependencies"
			case "git-path":
				requests[0].Path = "/workspace/.Git"
			case "symlink":
				if err := os.Symlink("/etc/passwd", filepath.Join(checkout, "dependencies", "link")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(checkout, "dependencies", "@scope", "file"), filepath.Join(checkout, "dependencies", "linked")); err != nil {
					t.Fatal(err)
				}
			case "empty":
				if err := os.Mkdir(filepath.Join(checkout, "empty"), 0700); err != nil {
					t.Fatal(err)
				}
				requests[0].Path = "/workspace/empty"
			case "empty-key":
				requests[0].Key = ""
			case "long-key":
				requests[0].Key = strings.Repeat("x", 513)
			case "too-many":
				for range 16 {
					requests = append(requests, requests[0])
				}
			}
			if scenario != "no-requests" {
				raw, _ := json.Marshal(requests)
				writeFixture(t, filepath.Join(runtimeRoot, "cache-requests.json"), string(raw), 0600)
			}
			goRoot, rustRoot := filepath.Join(root, "go-cache"), filepath.Join(root, "rust-cache")
			if scenario == "unmarked-root" || scenario == "bad-marker" {
				name, value := "unrelated", "keep"
				if scenario == "bad-marker" {
					name, value = ".co-cache-v1", "not-the-marker"
				}
				writeFixture(t, filepath.Join(goRoot, name), value, 0600)
				writeFixture(t, filepath.Join(rustRoot, name), value, 0600)
			}
			input := map[string]any{"mode": "cache", "root": rustRoot, "checkout": checkout, "runtime": runtimeRoot, "checkoutUrl": "https://git.co.codes/owner/repo.git", "image": "image@sha256:fixed", "publish": false}
			raw, _ := json.Marshal(input)
			command := exec.CommandContext(t.Context(), binary, "__migration-validate")
			command.Stdin = bytes.NewReader(append(raw, '\n'))
			data, err := command.Output()
			if err != nil {
				t.Fatal("cache probe failed", err)
			}
			var expected struct {
				OK           bool   `json:"ok"`
				Namespace    string `json:"namespace"`
				Publications []struct {
					ID    string `json:"id"`
					Bytes string `json:"bytes"`
				} `json:"publications"`
			}
			if err := json.Unmarshal(data, &expected); err != nil {
				t.Fatal(err)
			}
			store, err := openRunnerCache(goRoot, input["checkoutUrl"].(string), input["image"].(string))
			var publications []runnerCachePublication
			if err == nil {
				publications, err = store.collect(checkout, runtimeRoot)
			}
			if expected.OK != (err == nil) {
				t.Fatal("cache acceptance differs", scenario, err)
			}
			if err != nil {
				return
			}
			if filepath.Base(store.namespace) != expected.Namespace || len(publications) != len(expected.Publications) {
				t.Fatal("cache namespace or publication count differs")
			}
			for index, publication := range publications {
				if publication.id != expected.Publications[index].ID || base64.StdEncoding.EncodeToString(publication.data) != expected.Publications[index].Bytes {
					t.Fatal("cache bytes or identity differs", scenario)
				}
			}
			entries, _ := os.ReadDir(store.namespace)
			if len(entries) != 0 {
				t.Fatal("cache was published before successful completion")
			}
		})
	}
	t.Log("16 cache collection cases match compiled retained Rust namespaces, acceptance, IDs and exact COCA1/COAR1 bytes/modes")
}

func TestRunnerCachePublicationIsScopedImmutableAndBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix cache publication")
	}
	root := t.TempDir()
	store, err := openRunnerCache(root, "repository", "image")
	if err != nil {
		t.Fatal(err)
	}
	other, err := openRunnerCache(root, "other-repository", "image")
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := openRunnerCache(root, "repository\x00replacement", "image")
	if err != nil || store.namespace == other.namespace || store.namespace == replaced.namespace {
		t.Fatal("cache namespace crossed repository identity")
	}
	writeFixture(t, filepath.Join(other.namespace, strings.Repeat("a", 64)+".cache"), "keep", 0600)
	for index := range 32 {
		writeFixture(t, filepath.Join(store.namespace, runnerDigest([]byte{byte(index)})+".cache"), "old", 0600)
	}
	id := strings.Repeat("f", 64)
	publication := runnerCachePublication{store: store, id: id, data: []byte("new")}
	if err := publication.publish(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(store.namespace)
	others, _ := os.ReadDir(other.namespace)
	if len(entries) != 32 || len(others) != 1 {
		t.Fatal("namespace eviction altered unrelated cache entries")
	}
	publication.data = []byte("replacement")
	if err := publication.publish(); err != nil {
		t.Fatal(err)
	}
	stored, err := readRunnerCacheFile(filepath.Join(store.namespace, id+".cache"), 100)
	if err != nil || string(stored) != "new" {
		t.Fatal("cache publication overwrote immutable key", err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".cache") {
			t.Fatal("cache publication leaked a temporary file")
		}
	}
}
