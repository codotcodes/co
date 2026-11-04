package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func isolatedConfig(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "profile")
	t.Setenv("CO_CONFIG_DIR", root)
	return filepath.Join(root, "config.json")
}

func fullConfig(api string) configuration {
	config := configuration{}
	set := func(name string, value any) { config[name], _ = json.Marshal(value) }
	set("api_url", api)
	set("session_token", "fixture-human-session")
	set("upstream_name", "custom")
	set("jj", true)
	set("agents", []any{map[string]any{"id": "agent", "name": "fixture", "future": "retained"}})
	set("pending_agent_submissions", []any{map[string]any{"agent_id": "agent", "owner": "owner", "repo": "repo", "operations": []string{"pull", "push"}, "ttl_seconds": 3600, "reason": "fixture", "request_capability": "fixture-capability", "idempotency_key": "fixture-key"}})
	set("pending_agent_requests", []any{map[string]any{"id": "request", "agent_id": "agent", "owner": "owner", "repo": "repo", "poll_token": "fixture-poll", "operations": []string{"pull"}, "requested_expires_unix": 4000000000, "approval_expires_unix": 4000000000}})
	set("agent_grants", []any{map[string]any{"agent_id": "agent", "owner": "owner", "repo": "repo", "grant_id": "grant", "lineage_token": "fixture-lineage", "operations": []string{"pull", "push"}, "expires_unix": 4000000000}})
	set("machines", []any{map[string]any{"id": "machine", "scope": "owner", "api_url": "https://separate.example.invalid", "credential": "fixture-machine"}})
	set("future", map[string]any{"retained": true})
	return config
}

func writeConfigFixture(t *testing.T, path string, config configuration) {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, path, string(data), 0600)
}

func TestConfigPathsAndAPISelection(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CO_CONFIG_DIR", root)
	if path, err := configPath(); err != nil || path != filepath.Join(root, "config.json") {
		t.Fatal(path, err)
	}
	t.Setenv("CO_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if path, err := configPath(); err != nil || path != filepath.Join("co", "config.json") {
		t.Fatal(path, err)
	}
	t.Setenv("XDG_CONFIG_HOME", root)
	if path, _ := configPath(); path != filepath.Join(root, "co", "config.json") {
		t.Fatal(path)
	}
	t.Setenv("CO_API_URL", "")
	config := fullConfig("https://stored.invalid///")
	if effectiveAPI(config) != "" {
		t.Fatal("explicit empty API override fell back")
	}
	os.Unsetenv("CO_API_URL")
	if effectiveAPI(config) != "https://stored.invalid" {
		t.Fatal("stored API ignored")
	}
	config["api_url"] = json.RawMessage("null")
	if effectiveAPI(config) != "https://api.co.codes" {
		t.Fatal("wrong default API")
	}
}

func TestConfigSchemaFailsClosed(t *testing.T) {
	for _, source := range []string{`{"session_token":"\ud800"}`, `{"future":"\udc00"}`} {
		if _, err := parseConfig([]byte(source)); err == nil {
			t.Fatal("unpaired UTF-16 escape accepted")
		}
	}
	if _, err := parseConfig([]byte(`{"future":"\ud83d\ude00"}`)); err != nil {
		t.Fatal("valid UTF-16 pair rejected", err)
	}
	for _, source := range []string{`null`, `{"session_token":3}`, `{"session_token":"first","session_token":"second"}`, `{"agents":null}`, `{"jj":null}`, `{"agents":[{"name":"x"}]}`, `{"machines":[null]}`, `{"pending_agent_requests":[{"id":"request"}]}`, `{"agent_grants":[{"expires_unix":-1}]}`, `{"api_url":false}`, `{} {}`, string([]byte{'{', '"', 'x', '"', ':', '"', 255, '"', '}'})} {
		if _, err := parseConfig([]byte(source)); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	config, err := parseConfig([]byte(`{"agents":[{"id":"a","name":"n"}],"pending_agent_requests":[{"id":"r","agent_id":"a","owner":"o","repo":"r","poll_token":"poll","requested_expires_unix":18446744073709551615}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.stringValue("session_token") != nil || string(config["jj"]) != "false" || !bytes.Contains(config["pending_agent_requests"], []byte(`"operations":[]`)) {
		t.Fatal("schema defaults differ")
	}
}

func TestConfigAtomicMutationPreservesRecordsAndPermissions(t *testing.T) {
	path := isolatedConfig(t)
	writeConfigFixture(t, path, fullConfig("https://api.invalid"))
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := mutateConfig(func(config configuration) error { config["session_token"] = json.RawMessage("null"); return nil }); err != nil {
		t.Fatal(err)
	}
	config, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.stringValue("session_token") != nil {
		t.Fatal("session not removed")
	}
	for _, name := range []string{"agents", "pending_agent_submissions", "pending_agent_requests", "agent_grants", "machines", "future"} {
		var got, want any
		json.Unmarshal(config[name], &got)
		json.Unmarshal(fullConfig("https://api.invalid")[name], &want)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatal("credential record changed", name)
		}
	}
	if runtime.GOOS != "windows" {
		for name, want := range map[string]os.FileMode{path: 0600, path + ".lock": 0600, filepath.Dir(path): 0700} {
			info, err := os.Stat(name)
			if err != nil || info.Mode().Perm() != want {
				t.Fatal("unsafe config permissions", name, err)
			}
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := mutateConfig(func(config configuration) error {
		config["jj"] = json.RawMessage("false")
		return errors.New("fixture update refused")
	}); err == nil {
		t.Fatal("failed mutation succeeded")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed mutation changed committed config")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp"))
	if err != nil || len(files) != 0 {
		t.Fatal("temporary credentials leaked", err)
	}
}

func TestConcurrentConfigMutationsMergeUnderLock(t *testing.T) {
	isolatedConfig(t)
	var group sync.WaitGroup
	errors := make(chan error, 20)
	for index := range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			errors <- mutateConfig(func(config configuration) error {
				config[fmt.Sprintf("fixture-%d", index)] = json.RawMessage("true")
				return nil
			})
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	config, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for index := range 20 {
		if string(config[fmt.Sprintf("fixture-%d", index)]) != "true" {
			t.Fatal("locked update lost", index)
		}
	}
}
