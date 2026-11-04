package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func runnerFileOracle(t *testing.T, input any, output any) {
	t.Helper()
	binary := os.Getenv("CO_TEST_RUST_RUNNER_FILES_BINARY")
	if binary == "" {
		t.Skip("set the frozen retained Rust file-collection probe")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), binary, "__migration-validate")
	command.Stdin = bytes.NewReader(append(raw, '\n'))
	data, err := command.Output()
	if err != nil {
		t.Fatal("runner file probe failed", err)
	}
	if err := json.Unmarshal(data, output); err != nil {
		t.Fatal("runner file probe response failed", err)
	}
}

func TestRunnerArtifactsMatchCompiledRust(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix runner file collection")
	}
	for _, scenario := range []string{"deterministic", "binary", "empty", "bad-path", "bad-name", "bad-file-name", "symlink-root", "symlink-file", "hardlink", "oversize", "too-many"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			output := runnerArtifact{Name: "site", Path: "dist"}
			dist := filepath.Join(root, "dist")
			if err := os.MkdirAll(filepath.Join(dist, "assets"), 0700); err != nil {
				t.Fatal(err)
			}
			writeFixture(t, filepath.Join(dist, "index.html"), "<h1>Astro</h1>", 0600)
			writeFixture(t, filepath.Join(dist, "assets", "empty.css"), "", 0700)
			switch scenario {
			case "binary":
				writeFixture(t, filepath.Join(dist, "binary"), string([]byte{0, 255, 1, 127}), 0600)
			case "empty":
				if err := os.Mkdir(filepath.Join(root, "empty"), 0700); err != nil {
					t.Fatal(err)
				}
				output.Path = "empty"
			case "bad-path":
				output.Path = "../dist"
			case "bad-name":
				output.Name = "Site"
			case "bad-file-name":
				writeFixture(t, filepath.Join(dist, "bad name"), "bad", 0600)
			case "symlink-root":
				if err := os.Symlink("dist", filepath.Join(root, "linked")); err != nil {
					t.Fatal(err)
				}
				output.Path = "linked"
			case "symlink-file":
				if err := os.Symlink("index.html", filepath.Join(dist, "linked")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(dist, "index.html"), filepath.Join(dist, "linked")); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				file, err := os.Create(filepath.Join(dist, "huge"))
				if err != nil {
					t.Fatal(err)
				}
				err = file.Truncate(runnerArtifactLimit + 1)
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "too-many":
				for index := range 4096 {
					writeFixture(t, filepath.Join(dist, "many", strings.Repeat("x", index/1000)+string(rune('a'+index%26))+"-"+base64.RawURLEncoding.EncodeToString([]byte{byte(index >> 8), byte(index)})), "", 0600)
				}
			}
			var expected struct {
				OK    bool   `json:"ok"`
				Bytes string `json:"bytes"`
			}
			runnerFileOracle(t, map[string]any{"mode": "artifact", "directory": root, "output": output}, &expected)
			actual, err := collectRunnerArtifact(root, output)
			if expected.OK != (err == nil) || expected.OK && base64.StdEncoding.EncodeToString(actual) != expected.Bytes {
				t.Fatal("artifact differs from Rust", scenario, err)
			}
			if scenario == "deterministic" && runnerDigest(actual) != "62661a2f38380178a1c97e41472dcd809cd9ee3964d41d46a4aaf31a34cfc9f7" {
				t.Fatal("artifact manifest or payload changed")
			}
		})
	}
	t.Log("11 artifact collection scenarios match retained Rust acceptance and exact COAR1 bytes")
}

func TestRunnerActionOutputsMatchCompiledRust(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix runner file collection")
	}
	for _, scenario := range []string{"declared", "plain-secret", "encoded-secret", "case-secret", "unicode", "undeclared", "non-string", "null", "oversize-value", "oversize-total", "nul", "bad-keys", "duplicate-keys", "empty-keys", "symlink", "hardlink", "large-file"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "outputs.json")
			keys := []string{"url", "version_id"}
			secrets := []string{"secret/value", "日本語"}
			content := `{"url":"https://site.invalid","version_id":"v1"}`
			switch scenario {
			case "plain-secret":
				content = `{"url":"https://site.invalid/?token=secret/value","version_id":"v1"}`
			case "encoded-secret":
				content = `{"url":"https://site.invalid/?token=secret%2fvalue"}`
			case "case-secret":
				content = `{"url":"SECRET%2FVALUE"}`
			case "unicode":
				content = `{"url":"%e6%97%a5%e6%9c%ac%e8%aa%9e","version_id":"v1"}`
			case "undeclared":
				content = `{"other":"x"}`
			case "non-string":
				content = `{"url":1}`
			case "null":
				content = `{"url":null}`
			case "oversize-value":
				content = `{"url":"` + strings.Repeat("x", 4097) + `"}`
			case "oversize-total":
				keys = []string{"a", "b", "c", "d"}
				raw, _ := json.Marshal(map[string]string{"a": strings.Repeat("x", 4096), "b": strings.Repeat("x", 4096), "c": strings.Repeat("x", 4096), "d": strings.Repeat("x", 4096)})
				content = string(raw)
			case "nul":
				content = `{"url":"x\u0000x"}`
			case "bad-keys":
				keys = []string{"constructor"}
			case "duplicate-keys":
				keys = []string{"url", "url"}
			case "empty-keys":
				keys = []string{}
			case "large-file":
				content = strings.Repeat(" ", 65_537)
			}
			if scenario == "symlink" || scenario == "hardlink" {
				outside := filepath.Join(t.TempDir(), "outside")
				writeFixture(t, outside, content, 0600)
				var err error
				if scenario == "symlink" {
					err = os.Symlink(outside, path)
				} else {
					err = os.Link(outside, path)
				}
				if err != nil {
					t.Fatal(err)
				}
			} else {
				writeFixture(t, path, content, 0600)
			}
			var expected map[string]string
			runnerFileOracle(t, map[string]any{"mode": "outputs", "directory": root, "declared": keys, "secrets": secrets}, &expected)
			actual := collectRunnerActionOutputs(root, keys, secrets)
			if !reflect.DeepEqual(actual, expected) {
				t.Fatal("action outputs differ from Rust", scenario)
			}
		})
	}
	t.Log("17 action output scenarios match retained Rust bounds, declared keys, link rejection and encoded secret omission")
}
